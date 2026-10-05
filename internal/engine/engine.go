// Package engine executes a validated plan: renders targets with bounded
// concurrency, enforces idempotence via a state manifest, and performs
// all-or-nothing cleanup on failure or interruption.
package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/xingjianluo/scaffold-codegen-engine-go/internal/plan"
)

// ManifestDir is the per-root state directory name.
const ManifestDir = ".scaffold"

// DefaultManifestName is the manifest file name inside ManifestDir.
const DefaultManifestName = "manifest.json"

// Status is the classified outcome for a single target.
type Status string

const (
	// StatusCreated: file did not exist and was written.
	StatusCreated Status = "created"
	// StatusRecreated: file matched the previous manifest entry and was
	// rewritten with identical content (idempotent re-run).
	StatusRecreated Status = "recreated"
	// StatusVerified: file on disk already equals the rendered content;
	// nothing was written.
	StatusVerified Status = "verified"
	// StatusModified: file on disk differs from the previous manifest entry
	// (the user changed it); the file was left untouched.
	StatusModified Status = "modified"
	// StatusPreexisting: file exists with no manifest record; left untouched.
	StatusPreexisting Status = "preexisting"
	// StatusSkipped: a dependency failed or the run was interrupted.
	StatusSkipped Status = "skipped"
	// StatusFailed: rendering or writing failed.
	StatusFailed Status = "failed"
)

// Result is the per-target outcome recorded in the run report.
type Result struct {
	Target  string `json:"target"`
	Path    string `json:"path"`
	Status  Status `json:"status"`
	SHA256  string `json:"sha256,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Attempt int    `json:"attempt,omitempty"`
}

// Report is the full, deterministic outcome of one run.
type Report struct {
	PlanName  string         `json:"planName,omitempty"`
	Success   bool           `json:"success"`
	Order     []string       `json:"order"`
	Results   []Result       `json:"results"`
	Manifest  Manifest       `json:"manifest"`
	Recovered []string       `json:"recovered,omitempty"` // backups restored at start (interrupted earlier run)
	Error     *ClassifiedErr `json:"error,omitempty"`
}

// Error categories returned by Run.
const (
	KindInvalidPlan = "invalid_plan"
	KindManifest    = "manifest_error"
	KindRender      = "render_error"
	KindWrite       = "write_error"
	KindInterrupted = "interrupted"
	KindPartial     = "partial_failure"
)

// ClassifiedErr carries a machine-readable error category.
type ClassifiedErr struct {
	Kind    string       `json:"kind"`
	Message string       `json:"message"`
	Issues  []plan.Issue `json:"issues,omitempty"`
	Results []Result     `json:"results,omitempty"`
}

func (e *ClassifiedErr) Error() string {
	if len(e.Issues) > 0 {
		msgs := make([]string, len(e.Issues))
		for i, is := range e.Issues {
			if is.Target != "" {
				msgs[i] = string(is.Code) + " [" + is.Target + "]: " + is.Message
			} else {
				msgs[i] = string(is.Code) + ": " + is.Message
			}
		}
		return e.Kind + ": " + e.Message + ": " + strings.Join(msgs, "; ")
	}
	return e.Kind + ": " + e.Message
}

// Options configures a run.
type Options struct {
	// Workers is the maximum parallel rendering workers (>=1).
	Workers int
	// ManifestName is the manifest file name under .scaffold/.
	ManifestName string
	// WaitLock makes Run queue behind another run holding the workspace lock
	// instead of failing immediately with workspace_busy.
	WaitLock bool
}

// Engine runs plans against a root directory.
type Engine struct {
	root    string
	options Options

	mu        sync.Mutex // serializes whole runs against one Engine
	attemptMu sync.Mutex // guards attempt across worker goroutines within a run
	attempt   map[string]int
}

// New creates an Engine rooted at root.
func New(root string, opts Options) *Engine {
	if opts.Workers < 1 {
		opts.Workers = 1
	}
	if opts.ManifestName == "" {
		opts.ManifestName = DefaultManifestName
	}
	return &Engine{root: root, options: opts, attempt: map[string]int{}}
}

// Run validates and executes the plan.
//
// Coordination and durability:
//   - A cross-process exclusive flock on .scaffold/lock serializes runs from
//     multiple engines/processes/CI jobs on the same workspace. A second run
//     either fails immediately with workspace_busy (default) or queues behind
//     the holder (Options.WaitLock).
//   - Before any plan file is (re)written its pre-run bytes are captured once
//     in a run-local backup directory. On failure or interruption the backup
//     restores the workspace byte-for-byte to its pre-run state (created files
//     are deleted, overwritten files restored), so a retry starts from a
//     consistent state.
//   - state.json records every target's step and the run phase, surviving
//     normal completion, errors and hard interruption for later Status
//     queries.
//
// A successful run commits the new manifest atomically, then the state record,
// and removes the backup; a failed run never commits the manifest.
func (e *Engine) Run(ctx context.Context, p *plan.Plan) (*Report, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	order, issues := plan.ExecutionOrder(p)
	orderIDs := make([]string, 0, len(order))
	for _, t := range order {
		orderIDs = append(orderIDs, t.ID)
	}
	report := &Report{
		PlanName: p.NameSafe(),
		Order:    orderIDs,
		Results:  []Result{},
		Manifest: newEmptyManifest(),
	}
	if len(issues) > 0 {
		report.Error = &ClassifiedErr{
			Kind:    KindInvalidPlan,
			Message: "plan validation rejected the run; no files were touched",
			Issues:  issues,
		}
		return report, report.Error
	}

	stateDir := filepath.Join(e.root, ManifestDir)
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		report.Error = &ClassifiedErr{Kind: KindWrite, Message: err.Error()}
		return report, report.Error
	}

	// Exclusive cross-process lock before touching anything.
	holder := lockHolder{
		Mode:      "run",
		PlanName:  p.NameSafe(),
		PID:       os.Getpid(),
		Host:      hostName(),
		StartedAt: nowUTC().Format(time.RFC3339Nano),
	}
	lock, err := acquireWorkspaceLock(ctx, e.root, true, e.options.WaitLock, holder)
	if err != nil {
		if cerr, ok := err.(*ClassifiedErr); ok {
			report.Error = cerr
			return report, cerr
		}
		report.Error = &ClassifiedErr{Kind: KindWrite, Message: err.Error()}
		return report, report.Error
	}
	defer lock.release()

	// Recover from a previous process that died mid-run or mid-restore: if
	// the last state is non-terminal and a backup survives, restore it before
	// doing anything else. Committed-state backups are just garbage-collected.
	if err := e.recoverStale(stateDir, report); err != nil {
		report.Error = &ClassifiedErr{Kind: KindWrite, Message: err.Error()}
		return report, report.Error
	}

	manifest, err := loadManifest(stateDir, e.options.ManifestName)
	if err != nil {
		report.Error = &ClassifiedErr{Kind: KindManifest, Message: err.Error()}
		return report, report.Error
	}

	// This run's pre-write backup.
	runID := newRunID()
	bk, err := newBackup(e.root, runID)
	if err != nil {
		report.Error = &ClassifiedErr{Kind: KindWrite, Message: err.Error()}
		return report, report.Error
	}

	state := newRunState(p, order, holder)
	if err := state.save(stateDir, StateFileName); err != nil {
		report.Error = &ClassifiedErr{Kind: KindManifest, Message: err.Error()}
		_ = bk.remove()
		return report, report.Error
	}
	recorder := newStateRecorder(stateDir, StateFileName, state)

	// Run-local state shared by workers.
	var stateMu sync.Mutex
	rendered := make(map[string]string, len(order)) // target ID -> body
	staged := Manifest{Version: manifestVersion, Entries: map[string]FileEntry{}}
	var interrupted bool
	var firstErr *ClassifiedErr

	// classify a process-time failure deterministically.
	markFailure := func(kind, msg string) {
		stateMu.Lock()
		if firstErr == nil {
			firstErr = &ClassifiedErr{Kind: kind, Message: msg}
		}
		stateMu.Unlock()
	}

	sched := newScheduler(order, e.options.Workers)
	jobResults := sched.run(ctx, func(ctx context.Context, t *plan.Target) outcome {
		// An attempt starts as soon as the target is actually processed, so
		// the counter survives in the Engine even when a run rolls back.
		attempt := e.bumpAttempt(t.ID)
		_ = recorder.record(t, TargetState{Step: StepRendering, Attempt: attempt})
		if err := ctx.Err(); err != nil {
			markFailure(KindInterrupted, "run interrupted before processing target "+t.ID+": "+err.Error())
			stateMu.Lock()
			interrupted = true
			stateMu.Unlock()
			_ = recorder.record(t, TargetState{Step: StepTerminal, Status: StatusFailed,
				Reason: "interrupted: " + err.Error(), Attempt: attempt})
			return outcome{status: StatusFailed, reason: "interrupted: " + err.Error(), attempt: attempt}
		}

		// Render with dependency bodies available.
		deps := make(map[string]string, len(t.Deps))
		stateMu.Lock()
		for _, d := range t.Deps {
			deps[d] = rendered[d]
		}
		stateMu.Unlock()

		body, rerr := render(t, p, deps)
		if rerr != nil {
			markFailure(KindRender, rerr.Error())
			_ = recorder.record(t, TargetState{Step: StepTerminal, Status: StatusFailed,
				Reason: rerr.Error(), Attempt: attempt})
			return outcome{status: StatusFailed, reason: rerr.Error(), attempt: attempt}
		}
		sum := digest(body)
		_ = recorder.record(t, TargetState{Step: StepRendered, Status: "",
			SHA256: sum, Attempt: attempt})

		// File-system decision is serialized so concurrent workers can never
		// race on the same path.
		stateMu.Lock()
		defer stateMu.Unlock()

		status, reason, apply, werr := e.decide(t, body, sum, manifest, staged)
		if werr != nil {
			markFailure(KindWrite, werr.Error())
			_ = recorder.record(t, TargetState{Step: StepTerminal, Status: StatusFailed,
				Reason: werr.Error(), Attempt: attempt, SHA256: sum})
			return outcome{status: StatusFailed, reason: werr.Error(), attempt: attempt, sha: sum}
		}
		_ = recorder.record(t, TargetState{Step: StepDecided, Status: status,
			Reason: reason, Attempt: attempt, SHA256: sum})
		if apply {
			// Capture pre-run bytes before the first write to this path.
			if err := bk.captureIfNeeded(t.Path); err != nil {
				markFailure(KindWrite, err.Error())
				_ = recorder.record(t, TargetState{Step: StepTerminal, Status: StatusFailed,
					Reason: err.Error(), Attempt: attempt, SHA256: sum})
				return outcome{status: StatusFailed, reason: err.Error(), attempt: attempt, sha: sum}
			}
			if err := e.writeFile(t.Path, body); err != nil {
				markFailure(KindWrite, err.Error())
				_ = recorder.record(t, TargetState{Step: StepTerminal, Status: StatusFailed,
					Reason: err.Error(), Attempt: attempt, SHA256: sum})
				return outcome{status: StatusFailed, reason: err.Error(), attempt: attempt, sha: sum}
			}
			_ = recorder.record(t, TargetState{Step: StepWritten, Status: status,
				Reason: reason, Attempt: attempt, SHA256: sum})
		}
		rendered[t.ID] = body
		staged.Entries[t.Path] = FileEntry{Target: t.ID, Path: t.Path, SHA256: sum, Content: body}
		_ = recorder.record(t, TargetState{Step: StepTerminal, Status: status,
			Reason: reason, Attempt: attempt, SHA256: sum})
		return outcome{status: status, reason: reason, attempt: attempt, sha: sum}
	})

	// If the context expired while a render was in flight (the render itself
	// is not context-aware), surface it as an interruption here.
	if cerr := ctx.Err(); cerr != nil {
		stateMu.Lock()
		interrupted = true
		if firstErr == nil {
			firstErr = &ClassifiedErr{Kind: KindInterrupted, Message: "run interrupted: " + cerr.Error()}
		}
		stateMu.Unlock()
	}

	// Build deterministic results in topological order.
	results := make([]Result, 0, len(jobResults))
	failed := false
	for _, jr := range jobResults {
		if jr.status == StatusFailed || jr.status == StatusSkipped {
			failed = true
		}
		results = append(results, Result{
			Target: jr.targetID, Path: jr.path, Status: jr.status,
			SHA256: sched.shaOf(jr.targetID), Reason: jr.reason, Attempt: jr.attempt,
		})
	}
	report.Results = results

	if failed || interrupted || firstErr != nil {
		// Restore the workspace to exactly its pre-run state from the backup,
		// so no half-new / half-old files remain.
		_ = recorder.finish(PhaseRollingBack, firstErr, results)
		cleanupMsgs := bk.restore()

		kind := KindPartial
		msg := "one or more targets failed; the workspace was restored to its pre-run state and the manifest was not updated"
		if interrupted {
			kind = KindInterrupted
			msg = "run was interrupted; the workspace was restored to its pre-run state and the manifest was not updated"
		}
		if firstErr != nil && firstErr.Kind == KindRender {
			kind = KindRender
		}
		if len(cleanupMsgs) > 0 {
			for _, e := range cleanupMsgs {
				msg += "; restore error: " + e.Error()
			}
		}
		finalErr := &ClassifiedErr{Kind: kind, Message: msg, Results: results}
		// state.json stays describing the failed run and its terminal phase;
		// the backup is intentionally kept when restore reported errors so a
		// later run (or operator) can retry recovery.
		if len(cleanupMsgs) == 0 {
			phase := PhaseFailed
			if interrupted {
				phase = PhaseInterrupted
			}
			_ = recorder.finish(phase, finalErr, results)
			_ = bk.remove()
		} else {
			_ = recorder.finish(PhaseRollingBack, finalErr, results)
		}
		report.Error = finalErr
		return report, finalErr
	}

	// Success: merge the new entries over the old manifest. Entries for
	// targets present in this plan are replaced wholesale (dropping stale
	// paths when a target's output path changed); entries belonging to other
	// plans/targets are preserved.
	currentTargets := make(map[string]bool, len(order))
	for _, t := range order {
		currentTargets[t.ID] = true
	}
	merged := newEmptyManifest()
	for path, entry := range manifest.Entries {
		if currentTargets[entry.Target] {
			continue
		}
		merged.Entries[path] = entry
	}
	for path, entry := range staged.Entries {
		merged.Entries[path] = entry
	}
	if err := saveManifest(stateDir, e.options.ManifestName, merged); err != nil {
		cerr := &ClassifiedErr{Kind: KindManifest,
			Message: "commit failed after writing files: " + err.Error() + "; restoring the workspace from backup",
			Results: results}
		_ = recorder.finish(PhaseRollingBack, cerr, results)
		if rErrs := bk.restore(); len(rErrs) > 0 {
			for _, re := range rErrs {
				cerr.Message += "; restore error: " + re.Error()
			}
			_ = recorder.finish(PhaseRollingBack, cerr, results)
		} else {
			_ = recorder.finish(PhaseFailed, cerr, results)
			_ = bk.remove()
		}
		report.Error = cerr
		return report, cerr
	}
	// Manifest durable: success is committed. Record terminal state, then
	// drop the now-redundant backup.
	_ = recorder.finish(PhaseSucceeded, nil, results)
	_ = bk.remove()
	report.Success = true
	report.Manifest = merged
	return report, nil
}

// recoverStale handles leftovers from a process that died during a run. A
// non-terminal state together with a surviving backup means the workspace may
// contain partial writes: the oldest backup is restored. Backups whose run is
// known terminal (or which are superseded) are removed.
func (e *Engine) recoverStale(stateDir string, report *Report) error {
	stale, err := listStaleBackups(e.root)
	if err != nil {
		return fmt.Errorf("inspect run backups: %w", err)
	}
	if len(stale) == 0 {
		return nil
	}
	state, _ := loadRunState(stateDir, StateFileName)
	nonTerminal := state == nil || state.Phase == PhaseRunning || state.Phase == PhaseRollingBack
	if nonTerminal {
		// Restore from the oldest backup (the earliest unfinished run).
		b := backupFromDir(e.root, stale[0])
		if rErrs := b.restore(); len(rErrs) > 0 {
			var msgs []string
			for _, re := range rErrs {
				msgs = append(msgs, re.Error())
			}
			return fmt.Errorf("recover workspace from interrupted run %q: %s", filepath.Base(stale[0]),
				strings.Join(msgs, "; "))
		}
		report.Recovered = append(report.Recovered, filepath.Base(stale[0]))
	}
	for _, dir := range stale {
		_ = os.RemoveAll(dir)
	}
	return nil
}

// hostName returns the machine hostname best-effort for lock metadata.
func hostName() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return h
}

// newRunID returns a unique, sortable identifier for one run's backup dir.
func newRunID() string {
	return fmt.Sprintf("run-%d-%d", os.Getpid(), runCounter())
}

// decide classifies the action for a target without writing anything. apply
// is true when the file should be (re)written with body.
func (e *Engine) decide(t *plan.Target, body, sum string, old, staged Manifest) (status Status, reason string, apply bool, err error) {
	if prev, ok := staged.Entries[t.Path]; ok {
		// Cannot happen for a validated plan (unique paths); kept defensive.
		if prev.SHA256 != sum {
			return StatusFailed, "internal: path conflict within run", false,
				fmt.Errorf("path %q rendered twice with different content", t.Path)
		}
		return StatusVerified, "duplicate in-run render", false, nil
	}

	existing, readErr := os.ReadFile(filepath.Join(e.root, t.Path))
	switch {
	case errors.Is(readErr, os.ErrNotExist):
		return StatusCreated, "file did not exist; generated", true, nil
	case readErr != nil:
		return StatusFailed, "", false, fmt.Errorf("read %s: %w", t.Path, readErr)
	}

	if entry, known := old.Entries[t.Path]; known && entry.Target == t.ID {
		switch {
		case string(existing) != entry.Content:
			// User changed the generated file after the last successful run.
			return StatusModified,
				"file on disk was modified after the last committed run (disk sha256=" +
					digest(string(existing)) + ", manifest sha256=" + entry.SHA256 +
					"); leaving the file untouched", false, nil
		case entry.SHA256 == sum:
			// Unchanged and the new render is identical.
			return StatusVerified, "content already matches the plan; no write needed", false, nil
		default:
			// The plan changed output for an untouched file: deterministic overwrite.
			return StatusRecreated,
				"plan output changed (old sha256=" + entry.SHA256 + ", new sha256=" + sum +
					"); file rewritten with the new rendered content", true, nil
		}
	}

	// File exists but the manifest has never recorded it (or the path is
	// tracked for a different target): never touch it.
	return StatusPreexisting,
		"file already exists and is not tracked by manifest for this target (disk sha256=" +
			digest(string(existing)) + "); leaving it untouched", false, nil
}

// writeFile writes content to root/rel, creating parent directories.
func (e *Engine) writeFile(rel, content string) error {
	full := filepath.Join(e.root, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return fmt.Errorf("create directories for %s: %w", rel, err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", rel, err)
	}
	return nil
}

func (e *Engine) bumpAttempt(id string) int {
	e.attemptMu.Lock()
	defer e.attemptMu.Unlock()
	e.attempt[id]++
	return e.attempt[id]
}
