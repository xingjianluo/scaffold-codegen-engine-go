package engine

import (
	"context"
	"path/filepath"

	"github.com/xingjianluo/scaffold-codegen-engine-go/internal/plan"
)

// PreviewResult is the per-target conclusion of a dry run.
type PreviewResult struct {
	Target string `json:"target"`
	Path   string `json:"path"`
	Status Status `json:"status"`
	SHA256 string `json:"sha256,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// Preview is the full dry-run report. No file under the output root is
// created, modified or deleted while computing it (not even the workspace
// lock; see Engine.Preview).
type Preview struct {
	PlanName string          `json:"planName,omitempty"`
	Order    []string        `json:"order"`
	Results  []PreviewResult `json:"results"`
	Error    *ClassifiedErr  `json:"error,omitempty"`
}

// Preview validates the plan and renders every target in deterministic
// dependency order, classifying each path against the committed manifest and
// the current disk contents without writing anything.
//
// The per-target conclusions are computed by the exact same decision function
// as Run, and failures are propagated the same way (a target whose dependency
// fails is reported skipped without touching the filesystem), so a real
// execution starting immediately afterwards - with no intervening user change
// - yields identical per-target statuses. Preview takes no write lock and
// writes nothing; it refuses to start if an exclusive run is in progress,
// because its conclusions could otherwise race with that run's writes.
func (e *Engine) Preview(ctx context.Context, p *plan.Plan) (*Preview, error) {
	order, issues := plan.ExecutionOrder(p)
	orderIDs := make([]string, 0, len(order))
	for _, t := range order {
		orderIDs = append(orderIDs, t.ID)
	}
	preview := &Preview{PlanName: p.NameSafe(), Order: orderIDs, Results: []PreviewResult{}}
	if len(issues) > 0 {
		preview.Error = &ClassifiedErr{
			Kind:    KindInvalidPlan,
			Message: "plan validation rejected the preview; no files were touched",
			Issues:  issues,
		}
		return preview, preview.Error
	}

	// Read-only coordination: if a run holds the workspace exclusively, do
	// not race it. Never creates the lock file, so the workspace stays
	// untouched.
	readLock, busy := tryReadWorkspaceLock(e.root)
	if busy != nil {
		var ce *ClassifiedErr
		if cerr, ok := busy.(*ClassifiedErr); ok {
			ce = cerr
		} else {
			ce = &ClassifiedErr{Kind: KindWorkspaceBusy, Message: busy.Error()}
		}
		preview.Error = ce
		return preview, busy
	}
	if readLock != nil {
		defer readLock.release()
	}

	manifest, err := loadManifest(filepath.Join(e.root, ManifestDir), e.options.ManifestName)
	if err != nil {
		preview.Error = &ClassifiedErr{Kind: KindManifest, Message: err.Error()}
		return preview, preview.Error
	}

	// Sequential rendering mirrors the scheduler's skip propagation: a target
	// is skipped iff at least one dependency failed or was skipped.
	bodies := make(map[string]string, len(order))
	badDeps := make(map[string]bool, len(order))
	staged := newEmptyManifest() // defensive in-run duplicate detection
	results := make([]PreviewResult, 0, len(order))

	for _, t := range order {
		res := PreviewResult{Target: t.ID, Path: t.Path}
		if badDeps[t.ID] {
			res.Status = StatusSkipped
			res.Reason = "not executed: an upstream dependency would fail"
			results = append(results, res)
			propagateBad(t.ID, order, badDeps)
			continue
		}
		if cerr := ctx.Err(); cerr != nil {
			res.Status = StatusSkipped
			res.Reason = "not executed: preview interrupted: " + cerr.Error()
			results = append(results, res)
			propagateBad(t.ID, order, badDeps)
			continue
		}

		deps := map[string]string{}
		for _, d := range uniqueDeps(t.Deps) {
			deps[d] = bodies[d]
		}
		body, rerr := render(t, p, deps)
		if rerr != nil {
			res.Status = StatusFailed
			res.Reason = rerr.Error()
			results = append(results, res)
			propagateBad(t.ID, order, badDeps)
			continue
		}
		sum := digest(body)
		status, reason, _, derr := e.decide(t, body, sum, manifest, staged)
		if derr != nil {
			res.Status = StatusFailed
			res.Reason = derr.Error()
			results = append(results, res)
			propagateBad(t.ID, order, badDeps)
			continue
		}
		res.Status = status
		res.Reason = reason
		res.SHA256 = sum
		results = append(results, res)
		bodies[t.ID] = body
		staged.Entries[t.Path] = FileEntry{Target: t.ID, Path: t.Path, SHA256: sum, Content: body}
	}
	preview.Results = results

	// Mirror Run's terminal contract: a preview containing a failed target
	// (or a target skipped because of one) is itself an error, classified the
	// same way a real run would be. Nothing was written, so the message only
	// reports what would happen.
	for _, r := range results {
		if r.Status == StatusFailed {
			cerr := &ClassifiedErr{
				Kind:    KindRender,
				Message: "preview found targets that would fail; no files were touched",
				Results: previewResultsToResults(results),
			}
			preview.Error = cerr
			return preview, cerr
		}
	}
	return preview, nil
}

// previewResultsToResults adapts preview rows to the shared Result shape used
// in classified errors.
func previewResultsToResults(in []PreviewResult) []Result {
	out := make([]Result, 0, len(in))
	for _, r := range in {
		out = append(out, Result{
			Target: r.Target, Path: r.Path, Status: r.Status,
			SHA256: r.SHA256, Reason: r.Reason,
		})
	}
	return out
}

// propagateBad marks every direct (and transitive) dependent of id as having
// a bad dependency.
func propagateBad(id string, order []*plan.Target, bad map[string]bool) {
	for _, t := range order {
		for _, d := range uniqueDeps(t.Deps) {
			if d == id || bad[d] {
				bad[t.ID] = true
			}
		}
	}
}
