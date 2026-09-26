package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/xingjianluo/scaffold-codegen-engine-go/plan"
	"github.com/xingjianluo/scaffold-codegen-engine-go/state"
)

// ---- fixtures ------------------------------------------------------------

// fakeExec renders deterministic bytes without templates, and can inject
// failures, delays and concurrency probes.
type fakeExec struct {
	mu       sync.Mutex
	content  map[string]string // target -> content
	fail     map[string]bool
	delay    map[string]chan struct{} // closed to release a target render
	maxSeen  int
	active   int
	rendered []string
}

func newFakeExec() *fakeExec {
	return &fakeExec{content: map[string]string{}, fail: map[string]bool{}, delay: map[string]chan struct{}{}}
}

func (f *fakeExec) Render(t plan.Target) ([]byte, error) {
	f.mu.Lock()
	f.active++
	if f.active > f.maxSeen {
		f.maxSeen = f.active
	}
	f.rendered = append(f.rendered, t.Name)
	release := f.delay[t.Name]
	fail := f.fail[t.Name]
	content := f.content[t.Name]
	f.mu.Unlock()

	if release != nil {
		<-release
	}
	if fail {
		f.mu.Lock()
		f.active--
		f.mu.Unlock()
		return nil, fmt.Errorf("injected render failure for %s", t.Name)
	}
	f.mu.Lock()
	f.active--
	f.mu.Unlock()
	return []byte(content), nil
}

func (f *fakeExec) set(name, content string) *fakeExec { f.content[name] = content; return f }
func (f *fakeExec) setFail(name string) *fakeExec      { f.fail[name] = true; return f }
func (f *fakeExec) gate(name string) chan struct{} {
	ch := make(chan struct{})
	f.delay[name] = ch
	return ch
}

// writePlanFile writes templates and a JSON plan under dir.
func writePlanFile(t *testing.T, dir, jsonPlan string) string {
	t.Helper()
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		if err := os.WriteFile(filepath.Join(dir, n+".tpl"), []byte("tpl"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, "plan.json")
	if err := os.WriteFile(path, []byte(jsonPlan), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

const diamondPlan = `{
  "name": "diamond", "version": 1,
  "targets": [
    {"name": "a", "template": "a.tpl", "output_path": "out/a.txt"},
    {"name": "b", "template": "b.tpl", "output_path": "out/b.txt", "depends_on": ["a"]},
    {"name": "c", "template": "c.tpl", "output_path": "out/c.txt", "depends_on": ["a"]},
    {"name": "d", "template": "d.tpl", "output_path": "out/d.txt", "depends_on": ["b","c"]}
  ]
}`

func loadEngine(t *testing.T, dir string, exec Executor) (*Engine, *plan.Plan, *state.Store) {
	t.Helper()
	p, err := plan.Load(filepath.Join(dir, "plan.json"))
	if err != nil {
		t.Fatalf("load plan: %v", err)
	}
	st := state.NewStore(dir)
	return New(p, st, exec), p, st
}

func mustRun(t *testing.T, e *Engine, opts Options) *RunReport {
	t.Helper()
	rep, err := e.RunContext(context.Background(), opts)
	if err != nil {
		t.Fatalf("unexpected run error: %v", err)
	}
	return rep
}

// ---- tests ---------------------------------------------------------------

func TestIdempotentAndReproducible(t *testing.T) {
	dir := t.TempDir()
	writePlanFile(t, dir, diamondPlan)
	exec := newFakeExec().
		set("a", "A\n").set("b", "B\n").set("c", "C\n").set("d", "D\n")
	e, p, st := loadEngine(t, dir, exec)

	rep1 := mustRun(t, e, Options{Workers: 4})
	t.Logf("INPUT PLAN: %s", diamondPlan)
	t.Logf("PROCESSING ORDER: %v", rep1.Order)
	snapshot := map[string]string{}
	digests := map[string]string{}
	for _, r := range rep1.Results {
		body, err := os.ReadFile(filepath.Join(dir, r.Path))
		if err != nil {
			t.Fatal(err)
		}
		snapshot[r.Path] = string(body)
		digests[r.Path] = r.Digest
	}

	// Verify dependency order: a before b,c before d.
	idx := map[string]int{}
	for i, n := range rep1.Order {
		idx[n] = i
	}
	if !(idx["a"] < idx["b"] && idx["b"] < idx["d"] && idx["a"] < idx["c"] && idx["c"] < idx["d"]) {
		t.Fatalf("dependency order violated: %v", rep1.Order)
	}

	// Re-run: deterministic order, nothing rewritten, identical digests.
	for i := 0; i < 3; i++ {
		e2 := New(p, st, exec)
		rep := mustRun(t, e2, Options{Workers: 4})
		if !sameStrings(rep.Order, rep1.Order) {
			t.Fatalf("order changed across runs: %v vs %v", rep.Order, rep1.Order)
		}
		for _, r := range rep.Results {
			if r.Written {
				t.Fatalf("run %d rewrote %s", i+2, r.Name)
			}
			if r.Status != state.StatusUpToDate {
				t.Fatalf("run %d: %s status=%s reason=%s", i+2, r.Name, r.Status, r.Reason)
			}
			if r.Digest != digests[r.Path] {
				t.Fatalf("digest changed for %s", r.Name)
			}
			body, _ := os.ReadFile(filepath.Join(dir, r.Path))
			if string(body) != snapshot[r.Path] {
				t.Fatalf("content changed for %s", r.Path)
			}
		}
	}
	t.Logf("FINAL STATE: all outputs up_to_date, digests stable: %v", digests)
}

func TestConcurrentSchedulingAndSafety(t *testing.T) {
	dir := t.TempDir()
	writePlanFile(t, dir, diamondPlan)
	exec := newFakeExec().
		set("a", "A\n").set("b", "B\n").set("c", "C\n").set("d", "D\n")
	gb := exec.gate("b")
	gc := exec.gate("c")
	e, _, _ := loadEngine(t, dir, exec)

	done := make(chan struct{})
	go func() {
		_, _ = e.RunContext(context.Background(), Options{Workers: 4})
		close(done)
	}()
	// b and c must run concurrently after a.
	waitUntil(t, func() bool {
		exec.mu.Lock()
		defer exec.mu.Unlock()
		return exec.active == 2
	})
	exec.mu.Lock()
	if exec.maxSeen < 2 {
		exec.mu.Unlock()
		t.Fatal("expected at least two concurrent renders")
	}
	exec.mu.Unlock()
	close(gb)
	close(gc)
	<-done

	if exec.maxSeen > 3 {
		// a finished before b/c started; max can be 2 (b,c) then 1 (d).
		// allow up to 3 as slack.
		t.Fatalf("unexpected concurrency %d", exec.maxSeen)
	}
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 1000; i++ {
		if cond() {
			return
		}
		// brief sleep without importing time at top repeatedly
		sleep1ms()
	}
	t.Fatal("condition never became true")
}

// linearPlan a->b->c with three outputs
const linearPlan = `{
  "name": "linear", "version": 1,
  "targets": [
    {"name": "a", "template": "a.tpl", "output_path": "out/a.txt"},
    {"name": "b", "template": "b.tpl", "output_path": "out/b.txt", "depends_on": ["a"]},
    {"name": "c", "template": "c.tpl", "output_path": "out/c.txt", "depends_on": ["b"]}
  ]
}`

func TestUserModifiedRefusedAndForceOverwrites(t *testing.T) {
	dir := t.TempDir()
	writePlanFile(t, dir, linearPlan)
	exec := newFakeExec().set("a", "A\n").set("b", "B\n").set("c", "C\n")
	e, p, st := loadEngine(t, dir, exec)
	mustRun(t, e, Options{Workers: 3})

	// User edits b.
	bPath := filepath.Join(dir, "out/b.txt")
	if err := os.WriteFile(bPath, []byte("USER-EDIT\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Re-run must refuse, leave the edit intact and not touch other files.
	e2 := New(p, st, exec)
	rep, err := e2.RunContext(context.Background(), Options{Workers: 3})
	if err == nil {
		t.Fatal("expected conflict error")
	}
	te, ok := err.(*TaskError)
	if !ok || te.Kind != KindConflict {
		t.Fatalf("want KindConflict, got %T %v", err, err)
	}
	if !strings.Contains(te.Error(), "user_modified") {
		t.Fatalf("error must distinguish user modification: %v", err)
	}
	body, _ := os.ReadFile(bPath)
	if string(body) != "USER-EDIT\n" {
		t.Fatalf("user edit must survive: %q", body)
	}
	aBody, _ := os.ReadFile(filepath.Join(dir, "out/a.txt"))
	if string(aBody) != "A\n" {
		t.Fatal("unrelated target a must not be rewritten")
	}
	t.Logf("CONFLICT DIAGNOSIS: %v", te)
	for _, r := range rep.Results {
		t.Logf("final-status: target=%s status=%s skipped=%v reason=%q", r.Name, r.Status, r.Skipped, r.Reason)
	}

	// --force adopts the edited file and regenerates it.
	e3 := New(p, st, exec)
	rep3 := mustRun(t, e3, Options{Workers: 3, Force: true})
	body, _ = os.ReadFile(bPath)
	if string(body) != "B\n" {
		t.Fatalf("force must regenerate, got %q", body)
	}
	for _, r := range rep3.Results {
		if r.Name == "b" && !r.Written {
			t.Fatal("b should be written under --force")
		}
	}
}

func TestForeignFileRefused(t *testing.T) {
	dir := t.TempDir()
	writePlanFile(t, dir, linearPlan)
	// Pre-create an unrelated file at a's output.
	if err := os.MkdirAll(filepath.Join(dir, "out"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "out/a.txt"), []byte("hand-written\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	exec := newFakeExec().set("a", "A\n").set("b", "B\n").set("c", "C\n")
	e, _, _ := loadEngine(t, dir, exec)
	_, err := e.RunContext(context.Background(), Options{Workers: 3})
	if err == nil {
		t.Fatal("expected foreign-file conflict")
	}
	te := err.(*TaskError)
	if te.Kind != KindConflict || !strings.Contains(te.Error(), "foreign") {
		t.Fatalf("want foreign conflict, got %v", err)
	}
	body, _ := os.ReadFile(filepath.Join(dir, "out/a.txt"))
	if string(body) != "hand-written\n" {
		t.Fatal("foreign file must remain untouched")
	}
	t.Logf("FOREIGN DIAGNOSIS: %v", te)
}

func TestPartialFailureLeavesNoHalfStateAndRetryRecovers(t *testing.T) {
	dir := t.TempDir()
	writePlanFile(t, dir, linearPlan)
	exec := newFakeExec().set("a", "A\n").set("b", "B\n").set("c", "C\n").setFail("b")
	e, p, st := loadEngine(t, dir, exec)

	rep, err := e.RunContext(context.Background(), Options{Workers: 3})
	if err == nil {
		t.Fatal("expected injected failure")
	}
	te, ok := err.(*TaskError)
	if !ok || te.Kind != KindRender || te.Target != "b" {
		t.Fatalf("want render error on b, got %v", err)
	}
	// Nothing may have been installed in phase A; no outputs, no journal.
	if _, statErr := os.Stat(filepath.Join(dir, "out")); !os.IsNotExist(statErr) {
		entries, _ := os.ReadDir(filepath.Join(dir, "out"))
		if len(entries) != 0 {
			t.Fatalf("partial outputs left behind: %v", entries)
		}
	}
	if j, _ := st.ReadJournal(); j != nil {
		t.Fatal("journal must not exist after a phase-A failure")
	}
	// c must be marked skipped (upstream failed).
	var sawSkipped bool
	for _, r := range rep.Results {
		t.Logf("status after failure: target=%s status=%s skipped=%v reason=%q",
			r.Name, r.Status, r.Skipped, r.Reason)
		if r.Name == "c" && r.Skipped {
			sawSkipped = true
		}
	}
	if !sawSkipped {
		t.Fatal("downstream target c should be reported as skipped")
	}

	// Fix the failure and retry: complete, deterministic result.
	exec.fail["b"] = false
	e2 := New(p, st, exec)
	rep2 := mustRun(t, e2, Options{Workers: 3})
	for _, want := range []string{"a", "b", "c"} {
		body, err := os.ReadFile(filepath.Join(dir, "out", want+".txt"))
		if err != nil {
			t.Fatalf("retry did not produce %s: %v", want, err)
		}
		if string(body) != strings.ToUpper(want)+"\n" {
			t.Fatalf("retry produced wrong content for %s: %q", want, body)
		}
	}
	t.Logf("RETRY ORDER: %v -> final state up_to_date", rep2.Order)
}

func TestInterruptDuringInstallIsRecoverable(t *testing.T) {
	dir := t.TempDir()
	writePlanFile(t, dir, linearPlan)
	st := state.NewStore(dir)

	// Simulate a crash after writing the journal and installing a (fresh
	// output) and b (overwrote a backed-up file), but before the manifest.
	if err := os.MkdirAll(filepath.Join(dir, "out"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldB := filepath.Join(dir, "out", "b.txt")
	if err := os.WriteFile(oldB, []byte("OLD-B\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Prepare backups and staged files exactly like the engine would.
	stagedA := filepath.Join(dir, ".scaffold", "tmp", "stage-a")
	backupB := filepath.Join(dir, ".scaffold", "backups", "b.bak")
	os.MkdirAll(filepath.Dir(stagedA), 0o755)
	os.MkdirAll(filepath.Dir(backupB), 0o755)
	os.WriteFile(stagedA, []byte("A\n"), 0o644)
	os.WriteFile(backupB, []byte("OLD-B\n"), 0o644)
	// Install a and overwrite b; leave c absent (never reached).
	os.WriteFile(filepath.Join(dir, "out", "a.txt"), []byte("A\n"), 0o644)
	os.WriteFile(oldB, []byte("B\n"), 0o644)
	j := &state.Journal{Entries: []state.JournalEntry{
		{Name: "a", OutputPath: "out/a.txt", Staged: "tmp/stage-a"},
		{Name: "b", OutputPath: "out/b.txt", Backup: "backups/b.bak", WasTracked: false},
	}}
	if err := st.WriteJournal(j); err != nil {
		t.Fatal(err)
	}

	// The next engine run replays recovery before scheduling.
	exec := newFakeExec().set("a", "A\n").set("b", "B\n").set("c", "C\n")
	e, _, _ := loadEngine(t, dir, exec)
	// First run after crash: recovery restores the pre-run workspace, so the
	// foreign OLD-B is back and the engine refuses it (explainable result).
	_, err := e.RunContext(context.Background(), Options{Workers: 2})
	if err == nil {
		t.Fatal("expected foreign conflict after rollback restored OLD-B")
	}
	restoredB, _ := os.ReadFile(oldB)
	if string(restoredB) != "OLD-B\n" {
		t.Fatalf("rollback did not restore OLD-B: %q", restoredB)
	}
	if _, err := os.Stat(filepath.Join(dir, "out", "a.txt")); !os.IsNotExist(err) {
		t.Fatal("rollback must delete the freshly created a.txt")
	}
	t.Logf("post-crash state restored; rejection: %v", err)

	// User acknowledges with --force: full generation now commits.
	exec2 := newFakeExec().set("a", "A\n").set("b", "B\n").set("c", "C\n")
	e2, p2, st2 := loadEngine(t, dir, exec2)
	rep := mustRun(t, e2, Options{Workers: 2, Force: true})
	_ = p2
	_ = st2
	if rep.RecoveredFromInterruption != 0 {
		// journal was already consumed by the first recovering run
		t.Fatalf("journal should have been consumed, got %d", rep.RecoveredFromInterruption)
	}
	finalB, _ := os.ReadFile(oldB)
	if string(finalB) != "B\n" {
		t.Fatalf("final b = %q", finalB)
	}
	finalA, _ := os.ReadFile(filepath.Join(dir, "out", "a.txt"))
	if string(finalA) != "A\n" {
		t.Fatalf("final a = %q", finalA)
	}
	if jj, _ := st2.ReadJournal(); jj != nil {
		t.Fatal("journal must be cleared after commit")
	}
	t.Logf("RECOVERED then regenerated, order=%v", rep.Order)
}

func TestContextCancelInPhaseANoWorkspaceChange(t *testing.T) {
	dir := t.TempDir()
	writePlanFile(t, dir, linearPlan)
	exec := newFakeExec().set("a", "A\n").set("b", "B\n").set("c", "C\n")
	ga := exec.gate("a")
	e, _, st := loadEngine(t, dir, exec)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := e.RunContext(ctx, Options{Workers: 3})
		errCh <- err
	}()
	waitUntil(t, func() bool {
		exec.mu.Lock()
		defer exec.mu.Unlock()
		return exec.active >= 1
	})
	cancel()
	close(ga)
	err := <-errCh
	if err == nil {
		t.Fatal("expected interruption error")
	}
	if te, ok := err.(*TaskError); !ok || te.Kind != KindInterrupted {
		t.Fatalf("want interrupted, got %v", err)
	}
	// No real outputs and no journal.
	if _, err := os.Stat(filepath.Join(dir, "out", "a.txt")); !os.IsNotExist(err) {
		t.Fatal("phase-A cancellation must not install outputs")
	}
	if j, _ := st.ReadJournal(); j != nil {
		t.Fatal("no journal may exist after phase-A cancellation")
	}
}

func TestConcurrentEnginesSameWorkspace(t *testing.T) {
	dir := t.TempDir()
	writePlanFile(t, dir, diamondPlan)
	exec1 := newFakeExec().set("a", "A\n").set("b", "B\n").set("c", "C\n").set("d", "D\n")
	exec2 := newFakeExec().set("a", "A\n").set("b", "B\n").set("c", "C\n").set("d", "D\n")
	p, err := plan.Load(filepath.Join(dir, "plan.json"))
	if err != nil {
		t.Fatal(err)
	}
	st1, st2 := state.NewStore(dir), state.NewStore(dir)

	// Hold the first run inside phase A so the second attempt overlaps it.
	ga := exec1.gate("a")
	e1 := New(p, st1, exec1)
	e2 := New(p, st2, exec2)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, errs[0] = e1.RunContext(context.Background(), Options{Workers: 2})
	}()
	go func() {
		defer wg.Done()
		// Wait until the first run is active, then attempt concurrently.
		waitUntil(t, func() bool {
			exec1.mu.Lock()
			defer exec1.mu.Unlock()
			return exec1.active >= 1
		})
		_, errs[1] = e2.RunContext(context.Background(), Options{Workers: 2})
	}()
	// Give the second goroutine time to observe the active render and fail
	// on the lock, then release the gate.
	sleep1ms()
	sleep1ms()
	close(ga)
	wg.Wait()

	// Exactly one outcome: the holder succeeds, the overlapping run is
	// rejected with KindLocked (and wrote nothing).
	if errs[0] != nil {
		t.Fatalf("lock holder must succeed: %v", errs[0])
	}
	te, ok := errs[1].(*TaskError)
	if !ok || te.Kind != KindLocked {
		t.Fatalf("overlapping run must be KindLocked, got %v", errs[1])
	}
	t.Logf("CONCURRENT REJECTION: %v", te)

	// Final outputs are exactly the deterministic content.
	for _, want := range []string{"a:A\n", "b:B\n", "c:C\n", "d:D\n"} {
		parts := strings.SplitN(want, ":", 2)
		body, err := os.ReadFile(filepath.Join(dir, "out", parts[0]+".txt"))
		if err != nil {
			t.Fatalf("missing output %s: %v", parts[0], err)
		}
		if string(body) != parts[1] {
			t.Fatalf("output %s corrupted: %q", parts[0], body)
		}
	}
	// Follow-up run is clean/idempotent (lock was released).
	exec3 := newFakeExec().set("a", "A\n").set("b", "B\n").set("c", "C\n").set("d", "D\n")
	rep := mustRun(t, New(p, state.NewStore(dir), exec3), Options{Workers: 2})
	for _, r := range rep.Results {
		if r.Written || r.Status != state.StatusUpToDate {
			t.Fatalf("post-race state not clean: %+v", r)
		}
	}
}

func TestPlanChangedSafeRewrite(t *testing.T) {
	dir := t.TempDir()
	writePlanFile(t, dir, linearPlan)
	exec := newFakeExec().set("a", "A-v1\n").set("b", "B\n").set("c", "C\n")
	e, p, st := loadEngine(t, dir, exec)
	mustRun(t, e, Options{Workers: 3})

	// Plan now renders different content for a, but on-disk a is untouched.
	exec.content["a"] = "A-v2\n"
	e2 := New(p, st, exec)
	rep := mustRun(t, e2, Options{Workers: 3})
	var sawPlanChanged bool
	for _, r := range rep.Results {
		t.Logf("plan-change: target=%s status=%s written=%v", r.Name, r.Status, r.Written)
		if r.Name == "a" {
			if !r.Written {
				t.Fatal("a should be safely rewritten")
			}
			sawPlanChanged = true
		}
	}
	if !sawPlanChanged {
		t.Fatal("plan_changed target not reported")
	}
	body, _ := os.ReadFile(filepath.Join(dir, "out/a.txt"))
	if string(body) != "A-v2\n" {
		t.Fatalf("a = %q", body)
	}
	// b/c untouched and up to date.
	for _, n := range []string{"b", "c"} {
		b, _ := os.ReadFile(filepath.Join(dir, "out", n+".txt"))
		if string(b) != strings.ToUpper(n)+"\n" {
			t.Fatalf("%s changed unexpectedly", n)
		}
	}
}

func TestDeletedEngineOwnedFileRefused(t *testing.T) {
	dir := t.TempDir()
	writePlanFile(t, dir, linearPlan)
	exec := newFakeExec().set("a", "A\n").set("b", "B\n").set("c", "C\n")
	e, p, st := loadEngine(t, dir, exec)
	mustRun(t, e, Options{Workers: 3})

	if err := os.Remove(filepath.Join(dir, "out/b.txt")); err != nil {
		t.Fatal(err)
	}
	_, err := New(p, st, exec).RunContext(context.Background(), Options{Workers: 3})
	if err == nil {
		t.Fatal("deleting an engine-owned file must be refused")
	}
	te := err.(*TaskError)
	if te.Kind != KindConflict || !strings.Contains(te.Error(), "user_modified") {
		t.Fatalf("want user_modified for deletion, got %v", err)
	}
	// force recreates it.
	mustRun(t, New(p, st, exec), Options{Workers: 3, Force: true})
	b, _ := os.ReadFile(filepath.Join(dir, "out/b.txt"))
	if string(b) != "B\n" {
		t.Fatalf("b not recreated: %q", b)
	}
}
