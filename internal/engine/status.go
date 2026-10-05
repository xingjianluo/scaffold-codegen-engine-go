package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"

	"github.com/xingjianluo/scaffold-codegen-engine-go/internal/plan"
)

// WorkspaceState compares one target's artifact on disk against the
// committed manifest.
type WorkspaceState string

const (
	// WorkspaceConsistent: tracked by the manifest and disk matches it.
	WorkspaceConsistent WorkspaceState = "consistent"
	// WorkspaceModified: tracked by the manifest but the file on disk was
	// changed afterwards (by the user).
	WorkspaceModified WorkspaceState = "modified"
	// WorkspaceDeleted: tracked by the manifest but the file is gone.
	WorkspaceDeleted WorkspaceState = "deleted"
	// WorkspaceUntracked: the file exists but the manifest has no record of
	// it for this target (pre-existing, not managed).
	WorkspaceUntracked WorkspaceState = "untracked"
	// WorkspaceAbsent: neither tracked nor present on disk.
	WorkspaceAbsent WorkspaceState = "absent"
)

// TargetStatus is the always-queryable state of one plan target.
type TargetStatus struct {
	Target    string         `json:"target"`
	Path      string         `json:"path"`
	LastRun   Status         `json:"lastRun,omitempty"`
	Workspace WorkspaceState `json:"workspace"`
	Plan      Status         `json:"plan"`
	Detail    string         `json:"detail,omitempty"`
}

// StatusReport is the result of Inspect: where the last run stopped, whether
// the workspace still matches the manifest, and what a run would do now.
type StatusReport struct {
	PlanName   string         `json:"planName,omitempty"`
	Consistent bool           `json:"consistent"`
	LastRun    *RunState      `json:"lastRun,omitempty"`
	StalePaths []string       `json:"stalePaths,omitempty"`
	Targets    []TargetStatus `json:"targets"`
}

// Inspect reports the current state of the workspace against the plan. It is
// pure observation: it takes no lock and writes nothing, so it is safe to
// call at any time — after a successful run, a failed run, or an interrupted
// one (a killed run leaves the journal at phase "running").
//
// For every target it reports three orthogonal facts:
//   - LastRun: the final status the last journaled run recorded for it;
//   - Workspace: how the artifact on disk compares to the committed manifest
//     (a user edit is "modified", a removed file is "deleted" — never
//     conflated);
//   - Plan: what a run started right now would do with it (the dry-run
//     classification).
//
// Consistent is true exactly when every target is verified end-to-end: disk,
// manifest and plan all agree.
func (e *Engine) Inspect(ctx context.Context, p *plan.Plan) (*StatusReport, error) {
	order, issues := plan.ExecutionOrder(p)
	if len(issues) > 0 {
		return nil, &ClassifiedErr{
			Kind:    KindInvalidPlan,
			Message: "plan validation rejected the inspection",
			Issues:  issues,
		}
	}

	manifestDir := filepath.Join(e.root, ManifestDir)
	manifest, err := loadManifest(manifestDir, e.options.ManifestName)
	if err != nil {
		return nil, &ClassifiedErr{Kind: KindManifest, Message: err.Error()}
	}
	journal, err := loadJournal(manifestDir)
	if err != nil {
		return nil, &ClassifiedErr{Kind: KindManifest, Message: err.Error()}
	}

	// What would a run do right now? Reuse the dry-run path so the answer
	// can never drift from real execution. Render-level failures are not
	// fatal here: they are reported per target as Plan=failed.
	dry, derr := e.DryRun(ctx, p)
	planByTarget := map[string]Result{}
	if dry != nil {
		for _, r := range dry.Results {
			planByTarget[r.Target] = r
		}
	}

	lastByTarget := map[string]Status{}
	if journal != nil {
		for _, r := range journal.Results {
			lastByTarget[r.Target] = r.Status
		}
	}

	report := &StatusReport{
		PlanName: p.NameSafe(),
		LastRun:  journal,
		Targets:  make([]TargetStatus, 0, len(order)),
	}
	consistent := true
	for _, t := range order {
		ws, detail := e.workspaceState(t, manifest)
		ts := TargetStatus{
			Target:    t.ID,
			Path:      t.Path,
			LastRun:   lastByTarget[t.ID],
			Workspace: ws,
			Detail:    detail,
		}
		if pr, ok := planByTarget[t.ID]; ok {
			ts.Plan = pr.Status
			if pr.Reason != "" {
				if ts.Detail != "" {
					ts.Detail += "; "
				}
				ts.Detail += pr.Reason
			}
		} else {
			ts.Plan = StatusSkipped
		}
		if !(ws == WorkspaceConsistent && ts.Plan == StatusVerified) {
			consistent = false
		}
		report.Targets = append(report.Targets, ts)
	}
	report.Consistent = consistent

	// Paths the manifest tracks for targets that are no longer in this plan.
	inPlan := make(map[string]bool, len(order))
	for _, t := range order {
		inPlan[t.ID] = true
	}
	for path, entry := range manifest.Entries {
		if !inPlan[entry.Target] {
			report.StalePaths = append(report.StalePaths, path)
		}
	}
	sort.Strings(report.StalePaths)

	// A dry-run failure (e.g. a render error) is already reflected per
	// target as Plan=failed; the inspection itself succeeded.
	_ = derr
	return report, nil
}

// workspaceState compares one target's file on disk with the manifest.
func (e *Engine) workspaceState(t *plan.Target, manifest Manifest) (WorkspaceState, string) {
	entry, known := manifest.Entries[t.Path]
	tracked := known && entry.Target == t.ID
	existing, readErr := os.ReadFile(filepath.Join(e.root, t.Path))
	notExist := errors.Is(readErr, os.ErrNotExist)

	switch {
	case tracked && notExist:
		return WorkspaceDeleted,
			"tracked by the manifest (sha256=" + entry.SHA256 + ") but the file was deleted"
	case tracked && readErr == nil && string(existing) != entry.Content:
		return WorkspaceModified,
			"tracked by the manifest but changed on disk afterwards (disk sha256=" +
				digest(string(existing)) + ", manifest sha256=" + entry.SHA256 + ")"
	case tracked && readErr == nil:
		return WorkspaceConsistent, "on-disk content matches the committed manifest"
	case !tracked && readErr == nil:
		return WorkspaceUntracked,
			"file exists but is not managed for this target (disk sha256=" + digest(string(existing)) + ")"
	case notExist:
		return WorkspaceAbsent, "no artifact on disk and no manifest record"
	default:
		return WorkspaceAbsent, "unreadable: " + readErr.Error()
	}
}
