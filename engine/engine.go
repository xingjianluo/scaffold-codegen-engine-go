// Package engine schedules target rendering with bounded concurrency and
// commits outputs with a journal-protected two-phase protocol:
//
//  1. Phase A (concurrent, DAG-respecting): render every target, classify its
//     output against the last manifest and disk, and stage every change in a
//     private temp file with a backup of any existing output. The workspace's
//     real outputs are not touched yet, so an early failure leaves no trace.
//  2. Phase B (serial, deterministic): write the journal, install all staged
//     files, write the new manifest, then delete the journal.
//
// A crash/interruption in phase B is replayed on the next Run() using the
// surviving journal: newly created files are deleted and overwritten files
// restored, returning the workspace to the last committed state.
package engine

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"sync"
	"syscall"

	"github.com/xingjianluo/scaffold-codegen-engine-go/graph"
	"github.com/xingjianluo/scaffold-codegen-engine-go/plan"
	"github.com/xingjianluo/scaffold-codegen-engine-go/state"
)

// Executor renders content for one target.
type Executor interface {
	Render(t plan.Target) ([]byte, error)
}

// Options configures an Engine run.
type Options struct {
	// Workers is the maximum number of concurrently rendering targets.
	Workers int
	// Force adopts/overwrites foreign and user-modified files instead of
	// refusing them.
	Force bool
	// DryRun computes decisions and rendered content but writes nothing.
	DryRun bool
}

// TargetResult is the explainable outcome for one target.
type TargetResult struct {
	Name    string       `json:"name"`
	Path    string       `json:"path"`
	Status  state.Status `json:"status"`
	Skipped bool         `json:"skipped,omitempty"`
	Reason  string       `json:"reason"`
	Written bool         `json:"written"`
	Digest  string       `json:"digest,omitempty"`
}

// RunReport is the full result of one execution.
type RunReport struct {
	PlanName string `json:"plan_name"`
	// Order is the deterministic topological order.
	Order []string `json:"order"`
	// Results follows Order.
	Results []TargetResult `json:"results"`
	// FailedTarget names the target that aborted the run, empty on success.
	FailedTarget string `json:"failed_target,omitempty"`
	// RolledBack lists targets whose this-run outputs were undone.
	RolledBack []string `json:"rolled_back,omitempty"`
	// RecoveredFromInterruption is the number of journal entries replayed
	// from a previously interrupted run.
	RecoveredFromInterruption int `json:"recovered_from_interruption,omitempty"`
}

// Engine runs plans.
type Engine struct {
	p        *plan.Plan
	g        *graph.Graph
	store    *state.Store
	exec     Executor
	root     string
	manifest *state.Manifest
}

// New constructs an Engine from a validated plan.
func New(p *plan.Plan, store *state.Store, exec Executor) *Engine {
	return &Engine{p: p, store: store, exec: exec, root: store.Root()}
}

// Order exposes the deterministic execution order for diagnostics.
func (e *Engine) Order() []string { return e.g.Order() }

// Run executes the plan with SIGINT/SIGTERM triggering graceful cancellation.
func (e *Engine) Run(opts Options) (*RunReport, error) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return e.RunContext(ctx, opts)
}

// prepared is one target's fully staged phase-A result.
type prepared struct {
	res   TargetResult
	entry *state.JournalEntry // nil when nothing has to be installed
}

// RunContext executes the plan under ctx.
func (e *Engine) RunContext(ctx context.Context, opts Options) (*RunReport, error) {
	if opts.Workers < 1 {
		opts.Workers = 1
	}

	// One engine run per workspace at a time. Concurrent attempts fail fast
	// with an explainable, categorized error instead of racing on outputs.
	release, err := e.store.Lock()
	if err != nil {
		return nil, taskErr(KindLocked, "", "cannot start run", err)
	}
	defer release()

	// Replay a stale journal from an interrupted previous run first.
	n, err := e.recoverIfNeeded()
	if err != nil {
		return nil, taskErr(KindIO, "", "cannot recover interrupted run", err)
	}

	g, err := graph.Build(e.p)
	if err != nil {
		return nil, err
	}
	e.g = g
	m, err := e.store.Load()
	if err != nil {
		return nil, taskErr(KindIO, "", "cannot load manifest", err)
	}
	e.manifest = m

	order := g.Order()
	report := &RunReport{PlanName: e.p.Name, Order: order, RecoveredFromInterruption: n}

	// ---- Phase A: concurrent render + classify + stage --------------------
	byName := make(map[string]*plan.Target, len(e.p.Targets))
	indeg := make(map[string]int)
	consumers := make(map[string][]string)
	for i := range e.p.Targets {
		t := &e.p.Targets[i]
		byName[t.Name] = t
		indeg[t.Name] = len(t.DependsOn)
		for _, d := range t.DependsOn {
			consumers[d] = append(consumers[d], t.Name)
		}
	}
	for k := range consumers {
		sort.Strings(consumers[k])
	}

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		prepped  = map[string]*prepared{}
		entries  []state.JournalEntry
		failMu   sync.Mutex
		failed   bool
		failName string
		failE    error

		// progress is signaled whenever a worker finishes; interrupt wakes
		// the dispatcher so it can observe cancellation.
		progress = make(chan struct{}, 1)
	)
	signalProgress := func() {
		select {
		case progress <- struct{}{}:
		default:
		}
	}
	markFailed := func(name string, err error) {
		failMu.Lock()
		defer failMu.Unlock()
		if !failed {
			failed = true
			failName = name
			failE = err
			signalProgress()
		}
	}
	isFailed := func() bool {
		failMu.Lock()
		defer failMu.Unlock()
		return failed
	}

	sem := make(chan struct{}, opts.Workers)
	total := len(e.p.Targets)
	var readyMu sync.Mutex
	ready := []string{}
	for name, d := range indeg {
		if d == 0 {
			ready = insertSorted(ready, name)
		}
	}

	launch := func(name string) {
		wg.Add(1)
		go func() {
			acquired := false
			defer func() {
				if acquired {
					<-sem
				}
				wg.Done()
				signalProgress()
			}()
			select {
			case sem <- struct{}{}:
				acquired = true
			case <-ctx.Done():
				markFailed(name, taskErr(KindInterrupted, name, "run interrupted", ctx.Err()))
				return
			}

			if ctx.Err() != nil || isFailed() {
				markFailed(name, taskErr(KindInterrupted, name, "run interrupted", ctx.Err()))
				return
			}
			pr, err := e.prepare(ctx, *byName[name], opts)
			if err != nil {
				markFailed(name, err)
				return
			}
			mu.Lock()
			prepped[name] = pr
			if pr.entry != nil {
				entries = append(entries, *pr.entry)
			}
			mu.Unlock()

			// Promote now-ready dependents.
			readyMu.Lock()
			for _, c := range consumers[name] {
				indeg[c]--
				if indeg[c] == 0 {
					ready = insertSorted(ready, c)
				}
			}
			readyMu.Unlock()
		}()
	}

	// Dispatch until every target is launched or a failure is known.
	launched := map[string]bool{}
	for {
		readyMu.Lock()
		var name string
		if !isFailed() && ctx.Err() == nil && len(ready) > 0 {
			name = ready[0]
			ready = ready[1:]
		}
		readyMu.Unlock()
		if name != "" {
			if !launched[name] {
				launched[name] = true
				launch(name)
			}
			continue
		}
		// Nothing ready: either all targets are launched, or we must wait.
		if len(launched) >= total {
			break
		}
		if isFailed() {
			break
		}
		select {
		case <-progress:
		case <-ctx.Done():
			markFailed("", taskErr(KindInterrupted, "", "run interrupted", ctx.Err()))
		}
	}
	wg.Wait()

	// Deterministic, topological report.
	results := make([]TargetResult, 0, total)
	ran := map[string]bool{}
	for name, pr := range prepped {
		ran[name] = true
		results = append(results, pr.res)
	}
	sort.Slice(results, func(i, j int) bool {
		return indexOf(order, results[i].Name) < indexOf(order, results[j].Name)
	})
	for _, name := range order {
		if !ran[name] {
			reason := "skipped: an upstream target failed or was rejected"
			if ctx.Err() != nil {
				reason = "skipped: run interrupted"
			}
			results = append(results, TargetResult{
				Name: name, Path: byName[name].OutputPath,
				Status: state.StatusMissing, Skipped: true, Reason: reason,
			})
		}
	}
	// Re-sort since skipped append at tail.
	sort.SliceStable(results, func(i, j int) bool {
		return indexOf(order, results[i].Name) < indexOf(order, results[j].Name)
	})
	report.Results = results

	if failed || ctx.Err() != nil {
		// Phase A touched only staging/backups: discard them; the workspace
		// outputs and manifest are untouched.
		e.store.CleanupStaging()
		report.FailedTarget = failName
		if failE == nil {
			failE = taskErr(KindInterrupted, failName, "run interrupted", ctx.Err())
		}
		return report, failE
	}

	// ---- Dry run: nothing to commit --------------------------------------
	if opts.DryRun {
		e.store.CleanupStaging()
		return report, nil
	}

	// ---- Phase B: journal -> install -> manifest -> delete journal -------
	if err := e.commit(ctx, entries); err != nil {
		rolled := e.rollbackInMemory(entries)
		report.RolledBack = rolled
		return report, taskErr(KindIO, "", "commit failed; workspace rolled back", err)
	}
	for i := range report.Results {
		if prepped[report.Results[i].Name] != nil &&
			prepped[report.Results[i].Name].entry != nil {
			report.Results[i].Written = true
			report.Results[i].Status = state.StatusUpToDate
			report.Results[i].Reason = "written"
		}
	}
	return report, nil
}

// prepare renders, classifies, stages and backs up one target. It does not
// modify the real output path.
func (e *Engine) prepare(ctx context.Context, t plan.Target, opts Options) (*prepared, error) {
	if err := ctx.Err(); err != nil {
		return nil, taskErr(KindInterrupted, t.Name, "run interrupted", err)
	}
	desired, err := e.exec.Render(t)
	if err != nil {
		return nil, taskErr(KindRender, t.Name, "rendering failed", err)
	}
	st, reason, digest, err := e.classify(t, desired)
	if err != nil {
		return nil, taskErr(KindIO, t.Name, "classifying output failed", err)
	}
	res := TargetResult{Name: t.Name, Path: t.OutputPath, Status: st, Reason: reason, Digest: digest}

	switch st {
	case state.StatusUpToDate:
		return &prepared{res: res}, nil
	case state.StatusMissing, state.StatusPlanChanged:
		// safe to write
	case state.StatusUserModified, state.StatusForeign:
		if !opts.Force {
			return nil, taskErr(KindConflict, t.Name, string(st)+": "+reason, nil)
		}
		res.Reason = reason + "; overwritten with --force"
	}

	if opts.DryRun {
		res.Reason = reason + " (dry-run: nothing written)"
		return &prepared{res: res}, nil
	}

	staged, err := e.store.StageFile(t.Name, desired)
	if err != nil {
		return nil, taskErr(KindIO, t.Name, "staging failed", err)
	}
	abs := join(e.root, t.OutputPath)
	backup, err := e.store.BackupExisting(t.Name, abs)
	if err != nil {
		_ = os.Remove(staged)
		return nil, taskErr(KindIO, t.Name, "backup failed", err)
	}
	_, wasTracked := e.manifest.Targets[t.Name]
	entry := state.JournalEntry{
		Name:       t.Name,
		OutputPath: normalizeRel(t.OutputPath),
		Staged:     "tmp/" + baseName(staged),
		Backup:     backup,
		WasTracked: wasTracked,
	}
	res.Written = true
	return &prepared{res: res, entry: &entry}, nil
}

// commit performs phase B. All paths are distinct so installs are serial and
// deterministic (entries are sorted by output path).
func (e *Engine) commit(ctx context.Context, entries []state.JournalEntry) error {
	if len(entries) == 0 {
		// Still refresh the manifest (digests of unchanged outputs).
		return e.writeManifest()
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].OutputPath < entries[j].OutputPath })
	if err := e.store.WriteJournal(&state.Journal{Entries: entries}); err != nil {
		return err
	}
	for _, en := range entries {
		if ctx.Err() != nil {
			return taskErr(KindInterrupted, en.Name, "interrupted during install", ctx.Err())
		}
		staged := e.stagedAbs(en.Staged)
		abs := join(e.root, en.OutputPath)
		if err := e.store.Install(staged, abs); err != nil {
			return fmt.Errorf("install %s: %w", en.OutputPath, err)
		}
	}
	if err := e.writeManifest(); err != nil {
		return err
	}
	if err := e.store.RemoveJournal(); err != nil {
		return err
	}
	e.store.CleanupStaging()
	return nil
}

// writeManifest rebuilds the manifest from deterministically re-rendered
// content and the files now on disk.
func (e *Engine) writeManifest() error {
	nm := &state.Manifest{
		PlanName: e.p.Name, Version: e.p.Version,
		Targets: map[string]state.TargetState{},
	}
	for i := range e.p.Targets {
		t := &e.p.Targets[i]
		content, err := e.exec.Render(*t)
		if err != nil {
			return fmt.Errorf("re-render for manifest (%s): %w", t.Name, err)
		}
		abs := join(e.root, t.OutputPath)
		dig, err := state.FileDigest(abs)
		if err != nil {
			return fmt.Errorf("digest output (%s): %w", t.Name, err)
		}
		nm.Targets[t.Name] = state.TargetState{
			Name:          t.Name,
			OutputPath:    normalizeRel(t.OutputPath),
			ContentSHA256: dig,
			Rendered:      string(content),
			Mode:          uint32(state.OutputMode),
		}
	}
	return e.store.Save(nm)
}

// rollbackInMemory undoes already installed entries using the in-memory
// journal and removes the on-disk journal on success. Entries processed in
// reverse installation order.
func (e *Engine) rollbackInMemory(entries []state.JournalEntry) []string {
	sorted := append([]state.JournalEntry(nil), entries...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].OutputPath < sorted[j].OutputPath })
	var rolled []string
	ok := true
	for i := len(sorted) - 1; i >= 0; i-- {
		en := sorted[i]
		abs := join(e.root, en.OutputPath)
		if en.Backup != "" {
			if err := e.store.RestoreBackup(en.Backup, abs); err != nil {
				ok = false
				continue
			}
		} else {
			if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
				ok = false
				continue
			}
		}
		rolled = append(rolled, en.Name)
	}
	if ok {
		_ = e.store.RemoveJournal()
	}
	e.store.CleanupStaging()
	sort.Strings(rolled)
	return rolled
}

// recoverIfNeeded replays a leftover journal.
func (e *Engine) recoverIfNeeded() (int, error) {
	j, err := e.store.ReadJournal()
	if err != nil {
		return 0, err
	}
	if j == nil {
		return 0, nil
	}
	n, err := e.store.Recover(j)
	if err != nil {
		return n, err
	}
	e.store.CleanupStaging()
	if err := e.store.RemoveJournal(); err != nil {
		return n, err
	}
	return n, nil
}

func (e *Engine) stagedAbs(rel string) string {
	return join(join(e.store.Root(), ".scaffold"), rel)
}

func insertSorted(s []string, v string) []string {
	i := sort.SearchStrings(s, v)
	s = append(s, "")
	copy(s[i+1:], s[i:])
	s[i] = v
	return s
}

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return len(s)
}

func normalizeRel(p string) string {
	return filepathClean(p)
}
