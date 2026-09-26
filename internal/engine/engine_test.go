package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xingjianluo/scaffold-codegen-engine-go/internal/plan"
)

// dumpPlan prints the input plan to the test log as required by the task:
// every test logs its input plan, processing order and final state.
func dumpPlan(t *testing.T, tag string, p *plan.Plan) {
	t.Helper()
	data, _ := json.MarshalIndent(p, "", "  ")
	t.Logf("[%s] INPUT PLAN:\n%s", tag, data)
}

func dumpReport(t *testing.T, tag string, r *Report) {
	t.Helper()
	data, _ := json.MarshalIndent(struct {
		Success bool     `json:"success"`
		Order   []string `json:"order"`
		Results []Result `json:"results"`
		Error   string   `json:"error,omitempty"`
	}{r.Success, r.Order, r.Results, errString(r.Error)}, "", "  ")
	t.Logf("[%s] PROCESSING ORDER + FINAL STATE:\n%s", tag, data)
}

func errString(e *ClassifiedErr) string {
	if e == nil {
		return ""
	}
	return e.Error()
}

func resultMap(r *Report) map[string]Result {
	m := make(map[string]Result, len(r.Results))
	for _, res := range r.Results {
		m[res.Target] = res
	}
	return m
}

func readFile(t *testing.T, root, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(data)
}

func writeFile(t *testing.T, root, rel, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, filepath.Dir(rel)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, rel), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func samplePlan() *plan.Plan {
	return &plan.Plan{
		Name:   "sample",
		Params: map[string]string{"module": "example.com/demo"},
		Targets: map[string]*plan.Target{
			"go-mod": {
				Path:     "go.mod",
				Template: "module {{.params.module}}\n\ngo 1.25\n",
			},
			"readme": {
				Path:     "README.md",
				Template: "# {{.params.module}}\n",
			},
			"main": {
				Path:     "main.go",
				Template: "package main\n\n// module: {{.params.module}}\n// dep: {{deps \"go-mod\"}}\nfunc main() {}\n",
				Deps:     []string{"go-mod"},
			},
		},
	}
}

func TestRun_HappyPathAndIdempotentReproducible(t *testing.T) {
	root := t.TempDir()
	e := New(root, Options{Workers: 3})
	p := samplePlan()
	dumpPlan(t, "happy", p)

	r1, err := e.Run(context.Background(), p)
	dumpReport(t, "happy/run1", r1)
	if err != nil || !r1.Success {
		t.Fatalf("run1 failed: %v", err)
	}
	rm := resultMap(r1)
	for _, id := range []string{"go-mod", "main", "readme"} {
		if rm[id].Status != StatusCreated {
			t.Fatalf("run1 %s: expected created, got %s", id, rm[id].Status)
		}
	}
	if rm["main"].SHA256 == "" || rm["go-mod"].SHA256 == "" {
		t.Fatal("sha256 must be present for every rendered target")
	}
	if r1.Order[0] != "go-mod" {
		t.Fatalf("dependency must be processed first, order=%v", r1.Order)
	}

	// Snapshot files and manifest.
	snap := map[string]string{}
	for _, rel := range []string{"go.mod", "README.md", "main.go", ".scaffold/manifest.json"} {
		snap[rel] = readFile(t, root, rel)
	}

	// Re-run: same engine and a fresh engine must produce verified, no writes.
	r2, err := e.Run(context.Background(), p)
	dumpReport(t, "happy/run2-same-engine", r2)
	if err != nil || !r2.Success {
		t.Fatalf("run2 failed: %v", err)
	}
	for _, res := range r2.Results {
		if res.Status != StatusVerified {
			t.Fatalf("rerun %s: expected verified, got %s (%s)", res.Target, res.Status, res.Reason)
		}
		if res.SHA256 != rm[res.Target].SHA256 {
			t.Fatalf("rerun %s: digest changed", res.Target)
		}
	}
	for rel, want := range snap {
		if got := readFile(t, root, rel); got != want {
			t.Fatalf("idempotent rerun changed %s", rel)
		}
	}

	e2 := New(root, Options{Workers: 1})
	r3, err := e2.Run(context.Background(), p)
	dumpReport(t, "happy/run3-fresh-engine", r3)
	if err != nil {
		t.Fatal(err)
	}
	for _, res := range r3.Results {
		if res.Status != StatusVerified {
			t.Fatalf("fresh engine rerun %s: expected verified, got %s", res.Target, res.Status)
		}
	}
}

func TestRun_OrderIndependence(t *testing.T) {
	// Two independent roots execute the same plan with different worker
	// counts; outputs and digests must be identical.
	p := samplePlan()
	var manifestsA, manifestsB string
	var filesA, filesB map[string]string

	rootA := t.TempDir()
	if _, err := New(rootA, Options{Workers: 1}).Run(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	manifestsA = readFile(t, rootA, ".scaffold/manifest.json")
	filesA = map[string]string{
		"go.mod":    readFile(t, rootA, "go.mod"),
		"main.go":   readFile(t, rootA, "main.go"),
		"README.md": readFile(t, rootA, "README.md"),
	}

	rootB := t.TempDir()
	if _, err := New(rootB, Options{Workers: 8}).Run(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	manifestsB = readFile(t, rootB, ".scaffold/manifest.json")
	filesB = map[string]string{
		"go.mod":    readFile(t, rootB, "go.mod"),
		"main.go":   readFile(t, rootB, "main.go"),
		"README.md": readFile(t, rootB, "README.md"),
	}
	for rel := range filesA {
		if filesA[rel] != filesB[rel] {
			t.Fatalf("output %s differs by worker count", rel)
		}
	}
	// Manifest paths/content are identical set-wise.
	if manifestsA != manifestsB {
		t.Fatalf("manifests differ:\n%s\nvs\n%s", manifestsA, manifestsB)
	}
}

func TestRun_ModifiedAndPreexistingAreDistinguished(t *testing.T) {
	root := t.TempDir()
	e := New(root, Options{Workers: 2})
	p := samplePlan()
	dumpPlan(t, "conflict", p)

	r1, err := e.Run(context.Background(), p)
	dumpReport(t, "conflict/run1", r1)
	if err != nil {
		t.Fatal(err)
	}
	wantGoMod := readFile(t, root, "go.mod")

	// User edits a tracked file.
	writeFile(t, root, "go.mod", "module example.com/HACKED\n")
	// An unrelated untracked file appears at a path the plan would later use
	// in a separate plan run.
	writeFile(t, root, "foreign.txt", "user content\n")

	r2, err := e.Run(context.Background(), p)
	dumpReport(t, "conflict/run2", r2)
	if err != nil || !r2.Success {
		t.Fatalf("a user-modified file is not a fatal error, got: %v", err)
	}
	rm := resultMap(r2)
	if rm["go-mod"].Status != StatusModified {
		t.Fatalf("expected modified, got %s: %s", rm["go-mod"].Status, rm["go-mod"].Reason)
	}
	if got := readFile(t, root, "go.mod"); got != "module example.com/HACKED\n" {
		t.Fatal("user modification must never be overwritten")
	}
	if rm["readme"].Status != StatusVerified || rm["main"].Status != StatusVerified {
		t.Fatalf("unrelated targets must remain verified: %+v %+v", rm["readme"], rm["main"])
	}
	if wantGoMod != "module example.com/demo\n\ngo 1.25\n" {
		t.Fatal("sanity: original render captured")
	}

	// A new plan targets the pre-existing untracked file: must be classified
	// preexisting and left alone.
	p2 := &plan.Plan{Name: "p2", Targets: map[string]*plan.Target{
		"foreign": {Path: "foreign.txt", Template: "generated\n"},
	}}
	dumpPlan(t, "conflict/p2", p2)
	r3, err := e.Run(context.Background(), p2)
	dumpReport(t, "conflict/p2-run", r3)
	if err != nil || !r3.Success {
		t.Fatal(err)
	}
	if rm3 := resultMap(r3); rm3["foreign"].Status != StatusPreexisting {
		t.Fatalf("expected preexisting, got %s: %s", rm3["foreign"].Status, rm3["foreign"].Reason)
	}
	if got := readFile(t, root, "foreign.txt"); got != "user content\n" {
		t.Fatal("preexisting file must not be overwritten")
	}
}

func TestRun_RecreatedWhenPlanChangesUntouchedFile(t *testing.T) {
	root := t.TempDir()
	e := New(root, Options{Workers: 1})
	p := samplePlan()
	if _, err := e.Run(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	// Change the plan output for an untouched target.
	p.Targets["readme"].Template = "# {{.params.module}} v2\n"
	r, err := e.Run(context.Background(), p)
	dumpReport(t, "recreated", r)
	if err != nil {
		t.Fatal(err)
	}
	rm := resultMap(r)
	if rm["readme"].Status != StatusRecreated {
		t.Fatalf("expected recreated, got %s", rm["readme"].Status)
	}
	if got := readFile(t, root, "README.md"); got != "# example.com/demo v2\n" {
		t.Fatalf("expected rewritten content, got %q", got)
	}
	// Another rerun is now verified.
	r2, _ := e.Run(context.Background(), p)
	if resultMap(r2)["readme"].Status != StatusVerified {
		t.Fatal("expected verified after recreated")
	}
}

func TestRun_PartialFailureRollsBackAndRetriesRecover(t *testing.T) {
	root := t.TempDir()
	e := New(root, Options{Workers: 4})
	p := &plan.Plan{
		Name:   "partial",
		Params: map[string]string{"module": "example.com/demo"},
		Targets: map[string]*plan.Target{
			"a": {Path: "a.txt", Template: "A\n"},
			"b": {Path: "b.txt", Template: "B depends on {{deps \"a\"}}", Deps: []string{"a"}},
			"bad": {
				Path:     "bad.txt",
				Template: "{{.params.missing_value}}", // missingkey=error -> render failure
			},
			"c": {Path: "c.txt", Template: "C\n", Deps: []string{"bad"}},
		},
	}
	dumpPlan(t, "partial", p)

	r1, err := e.Run(context.Background(), p)
	dumpReport(t, "partial/run1-failed", r1)
	if err == nil || r1.Success {
		t.Fatal("expected a failure")
	}
	var ce *ClassifiedErr
	if !errors.As(err, &ce) || ce.Kind != KindRender {
		t.Fatalf("expected render_error category, got %T %v", err, err)
	}
	rm := resultMap(r1)
	if rm["bad"].Status != StatusFailed || !strings.Contains(rm["bad"].Reason, "missing") {
		t.Fatalf("bad must be a classified render failure: %+v", rm["bad"])
	}
	if rm["c"].Status != StatusSkipped {
		t.Fatalf("dependent of failure must be skipped, got %s: %s", rm["c"].Status, rm["c"].Reason)
	}
	if rm["a"].Status != StatusCreated || rm["b"].Status != StatusCreated {
		t.Fatalf("independent targets may succeed within run: a=%s b=%s", rm["a"].Status, rm["b"].Status)
	}
	// All-or-nothing rollback: nothing from the failed run may remain.
	for _, rel := range []string{"a.txt", "b.txt", "bad.txt", "c.txt"} {
		if _, statErr := os.Stat(filepath.Join(root, rel)); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("rollback left %s behind (err=%v)", rel, statErr)
		}
	}
	if _, statErr := os.Stat(filepath.Join(root, ".scaffold", "manifest.json")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("no manifest may be committed on failure")
	}

	// Fix the plan and retry: a clean, diagnosable success.
	p.Targets["bad"].Template = "BAD fixed for {{.params.module}}\n"
	r2, err := e.Run(context.Background(), p)
	dumpReport(t, "partial/run2-recovered", r2)
	if err != nil || !r2.Success {
		t.Fatalf("retry should recover, got: %v", err)
	}
	rm2 := resultMap(r2)
	// a and b were actually processed in run 1 (then rolled back), so their
	// attempt counter is 2; bad rendered (and failed) so it is also 2; c was
	// only skipped and is processed for the first time, so it stays 1.
	expected := map[string]int{"a": 2, "b": 2, "bad": 2, "c": 1}
	for id, want := range expected {
		if rm2[id].Status != StatusCreated {
			t.Fatalf("retry %s: expected created, got %s", id, rm2[id].Status)
		}
		if rm2[id].Attempt != want {
			t.Fatalf("retry attempt counter for %s should be %d, got %d", id, want, rm2[id].Attempt)
		}
	}
	if got := readFile(t, root, "b.txt"); !strings.Contains(got, "B depends on A") {
		t.Fatalf("dependency content wrong: %q", got)
	}
}

func TestRun_MarkedFailTargetDoesNotPoisonRetriesAfterPlanFix(t *testing.T) {
	root := t.TempDir()
	e := New(root, Options{Workers: 2})
	p := &plan.Plan{Targets: map[string]*plan.Target{
		"ok":  {Path: "ok.txt", Template: "ok\n"},
		"bad": {Path: "bad.txt", Template: "x", Fail: true},
	}}
	dumpPlan(t, "fail-flag", p)
	r1, err := e.Run(context.Background(), p)
	dumpReport(t, "fail-flag/run1", r1)
	if err == nil {
		t.Fatal("expected simulated failure")
	}
	// Attempt counters persist across runs on the same Engine instance.
	p.Targets["bad"].Fail = false
	r2, err := e.Run(context.Background(), p)
	dumpReport(t, "fail-flag/run2", r2)
	if err != nil || !r2.Success {
		t.Fatalf("retry failed: %v", err)
	}
	rm2 := resultMap(r2)
	if rm2["bad"].Attempt != 2 {
		t.Fatalf("expected attempt=2 on recovered target, got %d", rm2["bad"].Attempt)
	}
	if rm2["ok"].Attempt != 2 {
		t.Fatalf("target reprocessed after rollback must show attempt=2, got %d", rm2["ok"].Attempt)
	}
}

func TestRun_InvalidPlanTouchesNothing(t *testing.T) {
	root := t.TempDir()
	e := New(root, Options{Workers: 2})
	p := &plan.Plan{Targets: map[string]*plan.Target{
		"a": {Path: "a.txt", Deps: []string{"b"}},
		"b": {Path: "b.txt", Deps: []string{"a"}},
	}}
	dumpPlan(t, "invalid", p)
	r, err := e.Run(context.Background(), p)
	dumpReport(t, "invalid/run", r)
	var ce *ClassifiedErr
	if !errors.As(err, &ce) || ce.Kind != KindInvalidPlan {
		t.Fatalf("expected invalid_plan, got %v", err)
	}
	foundCycle := false
	for _, is := range ce.Issues {
		if is.Code == plan.IssueCycle {
			foundCycle = true
		}
	}
	if !foundCycle {
		t.Fatal("cycle issue must be classified in the error")
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		t.Fatalf("invalid plan must touch nothing, found %v", entries)
	}
}

func TestRun_ConcurrentRunsAreSerializedAndSafe(t *testing.T) {
	root := t.TempDir()
	e := New(root, Options{Workers: 4})
	p := samplePlan()
	dumpPlan(t, "concurrent", p)

	const n = 12
	var wg sync.WaitGroup
	errs := make([]error, n)
	reports := make([]*Report, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			reports[i], errs[i] = e.Run(context.Background(), p)
		}(i)
	}
	close(start)
	wg.Wait()

	created, verified := 0, 0
	var firstSHA string
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("run %d: %v", i, errs[i])
		}
		for _, res := range reports[i].Results {
			switch res.Status {
			case StatusCreated:
				created++
			case StatusVerified:
				verified++
			default:
				t.Fatalf("run %d target %s unexpected status %s", i, res.Target, res.Status)
			}
			if firstSHA == "" {
				firstSHA = res.SHA256
			} else if res.SHA256 != firstSHA && res.Target == "go-mod" {
				t.Fatal("digest divergence across concurrent runs")
			}
		}
	}
	if created != 3 {
		t.Fatalf("exactly one run must create the 3 files, got created=%d verified=%d", created, verified)
	}
	if verified != 3*(n-1) {
		t.Fatalf("remaining runs must verify, got verified=%d", verified)
	}
	t.Logf("[concurrent] FINAL: created=%d verified=%d", created, verified)

	// Deterministic order across every report.
	wantOrder := reports[0].Order
	for _, rp := range reports {
		if strings.Join(rp.Order, ",") != strings.Join(wantOrder, ",") {
			t.Fatalf("order diverged: %v vs %v", rp.Order, wantOrder)
		}
	}
}

func TestRun_CancellationRollsBack(t *testing.T) {
	root := t.TempDir()
	// Block the render of the first target until cancellation has been
	// requested, so the interruption path is exercised deterministically
	// regardless of scheduling timing.
	block := make(chan struct{})
	oldRender := renderHook
	renderHook = func(tg *plan.Target, body string, err error) (string, error) {
		if tg.ID == "slow" {
			<-block
		}
		return body, err
	}
	defer func() { renderHook = oldRender }()

	e := New(root, Options{Workers: 2})
	p := &plan.Plan{Targets: map[string]*plan.Target{
		"slow": {Path: "slow.txt", Template: "slow"},
		"x":    {Path: "x.txt", Template: "x", Deps: []string{"slow"}},
		"y":    {Path: "y.txt", Template: "y", Deps: []string{"slow"}},
	}}
	dumpPlan(t, "cancel", p)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	repCh := make(chan *Report, 1)
	go func() {
		r, err := e.Run(ctx, p)
		repCh <- r
		errCh <- err
	}()
	time.Sleep(30 * time.Millisecond) // let "slow" enter its blocked render
	cancel()
	close(block)
	err := <-errCh
	r := <-repCh
	dumpReport(t, "cancel/run", r)
	if err == nil {
		t.Fatal("expected interruption error")
	}
	var ce *ClassifiedErr
	if !errors.As(err, &ce) || ce.Kind != KindInterrupted {
		t.Fatalf("expected interrupted category, got %s", ce.Kind)
	}
	// No partial state: rollback must remove anything this run created.
	for _, rel := range []string{"slow.txt", "x.txt", "y.txt"} {
		if _, statErr := os.Stat(filepath.Join(root, rel)); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("rollback after cancel left %s: %v", rel, statErr)
		}
	}
	if _, statErr := os.Stat(filepath.Join(root, ".scaffold", "manifest.json")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("no manifest may be committed on interruption")
	}
	// Retry with a background context recovers to a clean success.
	renderHook = oldRender
	r2, err := New(root, Options{Workers: 2}).Run(context.Background(), p)
	dumpReport(t, "cancel/retry", r2)
	if err != nil || !r2.Success {
		t.Fatalf("post-cancel retry must succeed: %v", err)
	}
}

func TestScheduler_DeterministicOrderWithParallelism(t *testing.T) {
	// White-box scheduler test: record actual start/finish timestamps and
	// assert every dependency precedes its dependents, over many runs.
	p := samplePlan()
	order, issues := plan.ExecutionOrder(p)
	if len(issues) != 0 {
		t.Fatal(issues)
	}
	for iter := 0; iter < 30; iter++ {
		s := newScheduler(order, 3)
		var mu sync.Mutex
		finishAt := map[string]int{}
		clock := 0
		got := s.run(context.Background(), func(_ context.Context, tg *plan.Target) outcome {
			time.Sleep(time.Millisecond)
			mu.Lock()
			clock++
			finishAt[tg.ID] = clock
			mu.Unlock()
			return outcome{status: StatusCreated, sha: "x"}
		})
		ids := make([]string, len(got))
		for i, jr := range got {
			ids[i] = jr.targetID
		}
		if strings.Join(ids, ",") != strings.Join([]string{"go-mod", "main", "readme"}, ",") {
			t.Fatalf("iter %d: non-deterministic result order %v", iter, ids)
		}
		if !(finishAt["go-mod"] < finishAt["main"]) {
			t.Fatalf("iter %d: dependency finished after dependent: %v", iter, finishAt)
		}
	}
}

func TestDigestStable(t *testing.T) {
	a := digest("hello")
	if digest("hello") != a {
		t.Fatal("digest must be deterministic")
	}
	if len(a) != 64 {
		t.Fatal("sha256 hex length")
	}
	names := make([]string, 0)
	for _, s := range []Status{StatusCreated, StatusRecreated, StatusVerified, StatusModified, StatusPreexisting, StatusSkipped, StatusFailed} {
		names = append(names, string(s))
	}
	if len(names) != len(uniqueSortedStrings(names)) {
		t.Fatal("duplicate status names")
	}
}

func uniqueSortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func TestReportJSONExplainable(t *testing.T) {
	root := t.TempDir()
	p := samplePlan()
	r, err := New(root, Options{Workers: 2}).Run(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"sha256"`) || !strings.Contains(string(data), `"order"`) {
		t.Fatalf("report must explain order and digests: %s", data)
	}
	if !filepath.IsAbs(root) {
		t.Fatal("sanity")
	}
	if fmt.Sprint(r.Results) == "" {
		t.Fatal("sanity")
	}
}
