package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/xingjianluo/scaffold-codegen-engine-go/internal/plan"
)

// FileState classifies the current on-disk situation of one tracked or
// planned path. The two "user touched it" cases - deleted vs edited - are
// always reported separately.
type FileState string

const (
	// FileInSync: disk content equals what the committed manifest recorded
	// and the current plan renders the same bytes.
	FileInSync FileState = "in_sync"
	// FilePlanDrift: disk matches the last committed run, but the current
	// plan would render different content (a real run would recreate it).
	FilePlanDrift FileState = "plan_changed"
	// FileUserModified: disk differs from what the last successful run
	// committed (the user edited the generated file).
	FileUserModified FileState = "user_modified"
	// FileDeleted: a file committed by a tracked target no longer exists.
	FileDeleted FileState = "deleted"
	// FileUnexpected: a file exists at a tracked path but has no manifest
	// record for the owning target (never committed / owner mismatch).
	FileUnexpected FileState = "unexpected"
	// FilePlanOnly: the path is planned and absent from disk and has never
	// been committed (nothing generated yet).
	FilePlanOnly FileState = "planned_absent"
	// FileUnreadable: the path exists but cannot be read (permissions, etc.).
	FileUnreadable FileState = "unreadable"
)

// TargetStatus is one row of a status query.
type TargetStatus struct {
	Target  string    `json:"target"`
	Path    string    `json:"path"`
	LastRun Status    `json:"lastRun"`          // status recorded by the most recent run
	Step    string    `json:"step,omitempty"`   // step the target last reached
	Disk    FileState `json:"disk"`             // current disk vs manifest vs plan
	SHA256  string    `json:"sha256,omitempty"` // current disk or rendered digest
	Reason  string    `json:"reason,omitempty"`
}

// StatusReport answers: where did the last run stop, and does the workspace
// still match the plan.
type StatusReport struct {
	Phase       RunPhase       `json:"phase"`
	PlanName    string         `json:"planName,omitempty"`
	Order       []string       `json:"order"`
	Targets     []TargetStatus `json:"targets"`
	Consistent  bool           `json:"consistent"`  // disk matches the last committed run on every row
	MatchesPlan bool           `json:"matchesPlan"` // disk also equals what the current plan renders
	Stale       bool           `json:"stale"`       // last run died in a non-terminal phase
	Note        string         `json:"note,omitempty"`
	Error       *ClassifiedErr `json:"error,omitempty"`
}

// Status inspects the workspace without modifying it. It always succeeds in
// returning a report after normal, failed or interrupted runs; it never waits
// on an active run (state.json and manifest are read atomically). When p is
// non-nil the report additionally renders every target and compares the disk
// against the current plan; with p nil only committed manifest entries are
// inspected and plan-related fields stay zero.
func (e *Engine) Status(ctx context.Context, p *plan.Plan) (*StatusReport, error) {
	_ = ctx
	stateDir := filepath.Join(e.root, ManifestDir)

	state, err := loadRunState(stateDir, StateFileName)
	if err != nil {
		r := &StatusReport{Order: []string{}, Targets: []TargetStatus{}}
		r.Error = &ClassifiedErr{Kind: KindManifest, Message: err.Error()}
		return r, r.Error
	}
	manifest, err := loadManifest(stateDir, e.options.ManifestName)
	if err != nil {
		r := &StatusReport{Order: []string{}, Targets: []TargetStatus{}}
		r.Error = &ClassifiedErr{Kind: KindManifest, Message: err.Error()}
		return r, r.Error
	}

	var order []*plan.Target
	var planIssues plan.Issues
	var rendered map[string]string // target ID -> current rendered body
	if p != nil {
		order, planIssues = plan.ExecutionOrder(p)
		if len(planIssues) == 0 {
			rendered = renderAll(p, order)
		}
	}

	report := &StatusReport{Order: []string{}, Targets: []TargetStatus{}}
	if state != nil {
		report.Phase = state.Phase
		report.PlanName = state.PlanName
		report.Order = append(report.Order, state.Order...)
		report.Stale = state.Phase == PhaseRunning || state.Phase == PhaseRollingBack
	}

	// Build the deterministic row set: plan targets first (in order), then
	// manifest entries owned by no current target (sorted).
	seen := map[string]bool{}
	for _, t := range order {
		seen[t.ID] = true
		row := TargetStatus{Target: t.ID, Path: t.Path}
		fillLastRun(&row, state)
		var body string
		if rendered != nil {
			body = rendered[t.ID]
		}
		d, sha, why := classifyDisk(e.root, t.ID, t.Path, body, manifest)
		row.Disk = d
		row.SHA256 = firstNonEmpty(sha, row.SHA256)
		row.Reason = firstNonEmpty(why, row.Reason)
		report.Targets = append(report.Targets, row)
	}

	extra := make([]FileEntry, 0)
	for _, entry := range manifest.Entries {
		if !seen[entry.Target] {
			extra = append(extra, entry)
		}
	}
	sort.Slice(extra, func(i, j int) bool { return extra[i].Target < extra[j].Target })
	for _, entry := range extra {
		row := TargetStatus{Target: entry.Target, Path: entry.Path}
		fillLastRun(&row, state)
		d, sha, why := classifyDisk(e.root, entry.Target, entry.Path, "", manifest)
		row.Disk = d
		row.SHA256 = firstNonEmpty(sha, row.SHA256)
		row.Reason = firstNonEmpty(why, row.Reason)
		report.Targets = append(report.Targets, row)
	}

	// Determine consistency.
	report.Consistent = true
	report.MatchesPlan = true
	for _, row := range report.Targets {
		switch row.Disk {
		case FileInSync, FilePlanDrift:
			// plan_changed is still consistent with the last run, but no
			// longer matches the current plan.
		default:
			report.Consistent = false
		}
		if row.Disk != FileInSync {
			report.MatchesPlan = false
		}
	}
	// A plan that fails validation cannot be compared against.
	if len(planIssues) > 0 {
		report.MatchesPlan = false
		report.Note = "current plan does not validate; only last-run vs disk consistency was evaluated"
	}
	if report.Stale {
		if report.Consistent {
			report.Note = "the last run ended in a non-terminal phase but the workspace matches the last committed state; re-running is safe"
		} else {
			report.Note = "the last run ended in a non-terminal phase and the workspace diverged from the last committed state; a re-run will restore or diagnose it"
		}
	}
	return report, nil
}

// fillLastRun copies the last recorded run step/status into a row.
func fillLastRun(row *TargetStatus, state *RunState) {
	if state == nil {
		return
	}
	if ts, ok := state.Targets[row.Target]; ok && ts.Path == row.Path {
		row.LastRun = ts.Status
		row.Step = ts.Step
		if ts.SHA256 != "" {
			row.SHA256 = ts.SHA256
		}
		if ts.Reason != "" {
			row.Reason = ts.Reason
		}
	}
}

// firstNonEmpty returns the first non-empty string, or "".
func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// classifyDisk compares the disk, the manifest and the current plan render
// for one path. planBody is empty when no current plan is available.
func classifyDisk(root, targetID, rel, planBody string, manifest Manifest) (FileState, string, string) {
	full := filepath.Join(root, rel)
	data, err := os.ReadFile(full)
	missing := errors.Is(err, os.ErrNotExist)
	if err != nil && !missing {
		return FileUnreadable, "", fmt.Sprintf("cannot read %s: %v", rel, err)
	}
	entry, tracked := manifest.Entries[rel]
	owned := tracked && entry.Target == targetID
	diskSHA := ""
	if !missing {
		diskSHA = digest(string(data))
	}

	switch {
	case missing && owned:
		return FileDeleted, entry.SHA256,
			"committed file was deleted from disk (manifest sha256=" + entry.SHA256 + ")"
	case missing && !tracked:
		return FilePlanOnly, "",
			"path is planned but has never been generated"
	case missing && tracked && !owned:
		return FileDeleted, entry.SHA256,
			"file is missing; manifest records path under a different target (" + entry.Target + ")"
	case !missing && !owned:
		return FileUnexpected, diskSHA,
			"file exists but is not committed for this target (disk sha256=" + diskSHA + ")"
	case string(data) != entry.Content:
		return FileUserModified, diskSHA,
			"disk differs from the last committed run (disk sha256=" + diskSHA +
				", manifest sha256=" + entry.SHA256 + ")"
	case planBody != "":
		if want := digest(planBody); want != diskSHA {
			return FilePlanDrift, diskSHA,
				"disk matches the last run, but the current plan renders different content (disk sha256=" +
					diskSHA + ", plan sha256=" + want + ")"
		}
		return FileInSync, diskSHA, "disk matches the committed state and the current plan"
	default:
		return FileInSync, diskSHA, "disk matches the committed state"
	}
}

// renderAll renders every target in order, tolerating failures (a failed
// target yields no body, and the caller only uses bodies for comparison, not
// for deciding liveness). It never touches the filesystem.
func renderAll(p *plan.Plan, order []*plan.Target) map[string]string {
	bodies := make(map[string]string, len(order))
	bad := map[string]bool{}
	for _, t := range order {
		if bad[t.ID] {
			continue
		}
		deps := map[string]string{}
		ok := true
		for _, d := range uniqueDeps(t.Deps) {
			if b, present := bodies[d]; present {
				deps[d] = b
			} else {
				ok = false
			}
		}
		if !ok {
			propagateBad(t.ID, order, bad)
			continue
		}
		if body, err := render(t, p, deps); err == nil {
			bodies[t.ID] = body
		} else {
			propagateBad(t.ID, order, bad)
		}
	}
	return bodies
}
