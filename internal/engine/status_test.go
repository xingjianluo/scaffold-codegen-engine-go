package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/xingjianluo/scaffold-codegen-engine-go/internal/plan"
)

func dumpStatus(t *testing.T, tag string, r *StatusReport) {
	t.Helper()
	data, _ := json.MarshalIndent(r, "", "  ")
	t.Logf("[%s] STATUS:\n%s", tag, data)
}

func targetStatus(r *StatusReport, id string) TargetStatus {
	for _, ts := range r.Targets {
		if ts.Target == id {
			return ts
		}
	}
	return TargetStatus{}
}

// TestStatus_DistinguishesDeletedAndModified: after a successful run, delete
// one artifact and hand-edit another; Inspect must report the two situations
// as distinct, explainable states, and say what a run would do about them.
func TestStatus_DistinguishesDeletedAndModified(t *testing.T) {
	root := t.TempDir()
	e := New(root, Options{Workers: 2})
	p := samplePlan()
	dumpPlan(t, "status-drift", p)
	ctx := context.Background()

	// Before any run: everything absent, plan would create, no journal.
	pre, err := e.Inspect(ctx, p)
	dumpStatus(t, "status-drift/before-run", pre)
	if err != nil {
		t.Fatal(err)
	}
	if pre.Consistent {
		t.Fatal("empty workspace cannot be consistent with the plan")
	}
	if pre.LastRun != nil {
		t.Fatal("no run happened yet: last run must be absent")
	}
	if ts := targetStatus(pre, "go-mod"); ts.Workspace != WorkspaceAbsent || ts.Plan != StatusCreated {
		t.Fatalf("before run: %+v", ts)
	}

	if _, err := e.Run(ctx, p); err != nil {
		t.Fatal(err)
	}
	ok, err := e.Inspect(ctx, p)
	dumpStatus(t, "status-drift/after-run", ok)
	if err != nil {
		t.Fatal(err)
	}
	if !ok.Consistent {
		t.Fatal("right after a successful run everything must be consistent")
	}
	if ok.LastRun == nil || ok.LastRun.Phase != PhaseCommitted || !ok.LastRun.Success {
		t.Fatalf("journal must record the committed run: %+v", ok.LastRun)
	}
	for _, ts := range ok.Targets {
		if ts.Workspace != WorkspaceConsistent || ts.Plan != StatusVerified || ts.LastRun != StatusCreated {
			t.Fatalf("after run: %+v", ts)
		}
	}

	// Delete one artifact, hand-edit another, add a foreign file.
	if err := os.Remove(filepath.Join(root, "README.md")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "go.mod", "module example.com/HACKED\n")
	writeFile(t, root, "main.go", "package main // hand written\n")

	drift, err := e.Inspect(ctx, p)
	dumpStatus(t, "status-drift/drifted", drift)
	if err != nil {
		t.Fatal(err)
	}
	if drift.Consistent {
		t.Fatal("drifted workspace must not be consistent")
	}
	del := targetStatus(drift, "readme")
	if del.Workspace != WorkspaceDeleted {
		t.Fatalf("deleted file must be reported as deleted, got %s", del.Workspace)
	}
	if del.Plan != StatusCreated {
		t.Fatalf("a deleted artifact would be re-created, got %s", del.Plan)
	}
	mod := targetStatus(drift, "go-mod")
	if mod.Workspace != WorkspaceModified {
		t.Fatalf("user-edited file must be reported as modified, got %s", mod.Workspace)
	}
	if mod.Plan != StatusModified {
		t.Fatalf("a user-modified file would be left untouched (modified), got %s", mod.Plan)
	}
	main := targetStatus(drift, "main")
	if main.Workspace != WorkspaceModified || main.Plan != StatusModified {
		t.Fatalf("main.go: %+v", main)
	}
	if del.Workspace == mod.Workspace {
		t.Fatal("deleted and modified must never be conflated")
	}

	// Re-run: the deleted file is restored, the modified ones are kept.
	if _, err := e.Run(ctx, p); err != nil {
		t.Fatal(err)
	}
	after, err := e.Inspect(ctx, p)
	dumpStatus(t, "status-drift/rerun", after)
	if err != nil {
		t.Fatal(err)
	}
	if ts := targetStatus(after, "readme"); ts.Workspace != WorkspaceConsistent {
		t.Fatalf("re-created file must be consistent again: %+v", ts)
	}
	if ts := targetStatus(after, "go-mod"); ts.Workspace != WorkspaceModified {
		t.Fatalf("user edit must survive the re-run: %+v", ts)
	}
	if got := readFile(t, root, "go.mod"); got != "module example.com/HACKED\n" {
		t.Fatal("user modification must never be overwritten")
	}
}

// TestStatus_InterruptedRunLeavesQueryableState: a cancelled run rolls back
// and journals phase=rolled_back; Inspect then shows the workspace matching
// the (untouched) manifest again.
func TestStatus_InterruptedRunLeavesQueryableState(t *testing.T) {
	root := t.TempDir()
	e := New(root, Options{Workers: 2})
	p := samplePlan()
	ctx := context.Background()
	if _, err := e.Run(ctx, p); err != nil {
		t.Fatal(err)
	}

	// Interrupt the next run deterministically inside the first render.
	block := make(chan struct{})
	oldRender := renderHook
	renderHook = func(tg *plan.Target, body string, err error) (string, error) {
		if tg.ID == "go-mod" {
			<-block
		}
		return body, err
	}
	defer func() { renderHook = oldRender }()

	p2 := samplePlan()
	p2.Targets["readme"].Template = "# {{.params.module}} v2\n" // would recreate
	cctx, cancel := context.WithCancel(ctx)
	errCh := make(chan error, 1)
	go func() {
		_, err := e.Run(cctx, p2)
		errCh <- err
	}()
	time.Sleep(30 * time.Millisecond)
	cancel()
	close(block)
	if err := <-errCh; err == nil {
		t.Fatal("expected interruption")
	}
	renderHook = oldRender

	st, err := e.Inspect(ctx, p2)
	dumpStatus(t, "status-interrupted/after-cancel", st)
	if err != nil {
		t.Fatal(err)
	}
	if st.LastRun == nil || st.LastRun.Phase != PhaseRolledBack || st.LastRun.Success {
		t.Fatalf("journal must record the rolled-back run: %+v", st.LastRun)
	}
	// The workspace was restored to the pre-run state: readme still matches
	// the old manifest even though the new plan wants v2.
	ts := targetStatus(st, "readme")
	if ts.Workspace != WorkspaceConsistent {
		t.Fatalf("rolled-back file must match the old manifest again: %+v", ts)
	}
	if ts.Plan != StatusRecreated {
		t.Fatalf("plan still wants to recreate readme: %+v", ts)
	}
	if got := readFile(t, root, "README.md"); got != "# example.com/demo\n" {
		t.Fatalf("interrupted run must not leave new content behind, got %q", got)
	}

	// Re-running the same plan now converges to a clean committed state.
	if _, err := e.Run(ctx, p2); err != nil {
		t.Fatal(err)
	}
	final, err := e.Inspect(ctx, p2)
	dumpStatus(t, "status-interrupted/converged", final)
	if err != nil {
		t.Fatal(err)
	}
	if !final.Consistent {
		t.Fatal("after a clean re-run the workspace must be consistent")
	}
}

// TestRun_FailureRestoresOverwrittenFiles: a run that both creates new files
// and overwrites managed ones must, on failure, restore the workspace
// byte-for-byte: created files gone, overwritten files back to their
// pre-run content, manifest untouched.
func TestRun_FailureRestoresOverwrittenFiles(t *testing.T) {
	root := t.TempDir()
	e := New(root, Options{Workers: 3})
	ctx := context.Background()

	p1 := samplePlan()
	dumpPlan(t, "restore/v1", p1)
	if _, err := e.Run(ctx, p1); err != nil {
		t.Fatal(err)
	}
	preRun := dirSnapshot(t, root)
	// The journal and the lock file are run bookkeeping, not artifacts: the
	// journal is expected to record the rolled-back run. Everything else —
	// above all .scaffold/manifest.json — must be restored byte-for-byte.
	delete(preRun, ManifestDir+"/"+journalName)
	delete(preRun, ManifestDir+"/"+lockFileName)

	// v2: readme changes (would recreate), a new file appears (would
	// create), and one target fails.
	p2 := samplePlan()
	p2.Targets["readme"].Template = "# {{.params.module}} v2\n"
	p2.Targets["extra"] = &plan.Target{Path: "extra.txt", Template: "EXTRA\n"}
	p2.Targets["bad"] = &plan.Target{Path: "bad.txt", Template: "x", Fail: true}
	dumpPlan(t, "restore/v2-failing", p2)

	r, err := e.Run(ctx, p2)
	dumpReport(t, "restore/failed-run", r)
	if err == nil {
		t.Fatal("expected failure")
	}
	postRun := dirSnapshot(t, root)
	delete(postRun, ManifestDir+"/"+journalName)
	delete(postRun, ManifestDir+"/"+lockFileName)
	if len(postRun) != len(preRun) {
		t.Fatalf("workspace file set changed: before=%v after=%v", keysOf(preRun), keysOf(postRun))
	}
	for rel, want := range preRun {
		if postRun[rel] != want {
			t.Fatalf("file %s was not restored:\nbefore %q\nafter  %q", rel, want, postRun[rel])
		}
	}

	// The journal tells the story; status agrees the workspace matches the
	// old manifest (so the v2 plan would recreate readme again).
	st, serr := e.Inspect(ctx, p2)
	dumpStatus(t, "restore/status", st)
	if serr != nil {
		t.Fatal(serr)
	}
	if st.LastRun == nil || st.LastRun.Phase != PhaseRolledBack {
		t.Fatalf("journal phase: %+v", st.LastRun)
	}
	if ts := targetStatus(st, "readme"); ts.Workspace != WorkspaceConsistent || ts.Plan != StatusRecreated {
		t.Fatalf("readme after rollback: %+v", ts)
	}

	// Fix the plan and re-run: converges to a diagnosable success.
	p2.Targets["bad"].Fail = false
	p2.Targets["bad"].Template = "FIXED\n"
	r2, err := e.Run(ctx, p2)
	dumpReport(t, "restore/recovered", r2)
	if err != nil || !r2.Success {
		t.Fatalf("re-run must converge: %v", err)
	}
	rm := resultMap(r2)
	if rm["readme"].Status != StatusRecreated || rm["extra"].Status != StatusCreated || rm["bad"].Status != StatusCreated {
		t.Fatalf("unexpected recovery statuses: %+v", rm)
	}
	final, _ := e.Inspect(ctx, p2)
	dumpStatus(t, "restore/final", final)
	if !final.Consistent {
		t.Fatal("after recovery the workspace must be fully consistent")
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestRun_ConcurrentEnginesSerializeViaLock: two Engine instances (as in two
// processes sharing one workspace) cannot interleave: the later one queues
// on the lock, then observes the committed state and verifies.
func TestRun_ConcurrentEnginesSerializeViaLock(t *testing.T) {
	root := t.TempDir()
	p := samplePlan()
	dumpPlan(t, "lock", p)

	// Hold the first run inside the render of go-mod so the second run
	// definitely arrives while the lock is taken.
	gate := make(chan struct{})
	oldRender := renderHook
	renderHook = func(tg *plan.Target, body string, err error) (string, error) {
		if tg.ID == "go-mod" {
			<-gate
		}
		return body, err
	}
	defer func() { renderHook = oldRender }()

	e1 := New(root, Options{Workers: 2})
	e2 := New(root, Options{Workers: 2, NoWait: true})

	// e2 with NoWait must get an explicit "locked" conclusion, not corrupt
	// state and not queue.
	type outcome struct {
		report *Report
		err    error
	}
	first := make(chan outcome, 1)
	go func() {
		r, err := e1.Run(context.Background(), p)
		first <- outcome{r, err}
	}()
	time.Sleep(30 * time.Millisecond) // e1 holds the lock now

	r2, err2 := e2.Run(context.Background(), p)
	dumpReport(t, "lock/no-wait", r2)
	if err2 == nil {
		t.Fatal("no-wait run on a locked workspace must fail")
	}
	var ce *ClassifiedErr
	if !errors.As(err2, &ce) || ce.Kind != KindLocked {
		t.Fatalf("expected locked category, got %v", err2)
	}

	close(gate)
	o1 := <-first
	dumpReport(t, "lock/first", o1.report)
	if o1.err != nil {
		t.Fatal(o1.err)
	}
	renderHook = oldRender

	// A queueing run (default) now succeeds and verifies everything.
	r3, err3 := New(root, Options{Workers: 2}).Run(context.Background(), p)
	dumpReport(t, "lock/queued-after", r3)
	if err3 != nil {
		t.Fatal(err3)
	}
	for _, res := range r3.Results {
		if res.Status != StatusVerified {
			t.Fatalf("after the first run committed, later runs verify: %+v", res)
		}
	}
}

// TestRun_LockReleasedOnFailure: a failed run must release the lock so the
// next pipeline is not stuck.
func TestRun_LockReleasedOnFailure(t *testing.T) {
	root := t.TempDir()
	e := New(root, Options{Workers: 1})
	p := &plan.Plan{Targets: map[string]*plan.Target{
		"bad": {Path: "bad.txt", Template: "x", Fail: true},
	}}
	if _, err := e.Run(context.Background(), p); err == nil {
		t.Fatal("expected failure")
	}
	// Immediately re-runnable with NoWait: the lock must be free.
	e2 := New(root, Options{Workers: 1, NoWait: true})
	p.Targets["bad"].Fail = false
	p.Targets["bad"].Template = "ok\n"
	r, err := e2.Run(context.Background(), p)
	dumpReport(t, "lock-released", r)
	if err != nil || !r.Success {
		t.Fatalf("run after a failure must not see a stale lock: %v", err)
	}
}

// TestRun_ParallelContendersExactlyOneCreates: many engines race on one
// workspace; exactly one creates each file, all converge, journal/manifest
// stay coherent.
func TestRun_ParallelContendersExactlyOneCreates(t *testing.T) {
	root := t.TempDir()
	p := samplePlan()
	const n = 8
	var wg sync.WaitGroup
	reports := make([]*Report, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			e := New(root, Options{Workers: 2})
			reports[i], errs[i] = e.Run(context.Background(), p)
		}(i)
	}
	wg.Wait()
	created := 0
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("contender %d: %v", i, errs[i])
		}
		for _, res := range reports[i].Results {
			if res.Status == StatusCreated {
				created++
			} else if res.Status != StatusVerified {
				t.Fatalf("contender %d unexpected status %+v", i, res)
			}
		}
	}
	if created != 3 {
		t.Fatalf("exactly one run may create the 3 files, got %d", created)
	}
	st, err := New(root, Options{}).Inspect(context.Background(), p)
	dumpStatus(t, "contenders/final", st)
	if err != nil || !st.Consistent {
		t.Fatalf("final state must be consistent: %v", err)
	}
	if st.LastRun == nil || st.LastRun.Phase != PhaseCommitted {
		t.Fatalf("journal must show a committed last run: %+v", st.LastRun)
	}
}
