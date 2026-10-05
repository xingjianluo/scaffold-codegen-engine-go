package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/xingjianluo/scaffold-codegen-engine-go/internal/plan"
)

// statuses extracts target -> status for concise comparison.
func statuses(r *Report) map[string]Status {
	m := make(map[string]Status, len(r.Results))
	for _, res := range r.Results {
		m[res.Target] = res.Status
	}
	return m
}

// dirSnapshot lists all files (relative paths) under root, sorted-ish via map.
func dirSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		out[filepath.ToSlash(rel)] = string(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func assertSameStatuses(t *testing.T, tag string, dry, real *Report) {
	t.Helper()
	ds, rs := statuses(dry), statuses(real)
	if len(ds) != len(rs) {
		t.Fatalf("[%s] target count differs: dry=%v real=%v", tag, ds, rs)
	}
	for id, want := range ds {
		if got := rs[id]; got != want {
			t.Fatalf("[%s] target %s: dry-run said %s, real run said %s", tag, id, want, got)
		}
	}
	if len(dry.Order) != len(real.Order) {
		t.Fatalf("[%s] order length differs", tag)
	}
	for i := range dry.Order {
		if dry.Order[i] != real.Order[i] {
			t.Fatalf("[%s] order differs: dry=%v real=%v", tag, dry.Order, real.Order)
		}
	}
}

// TestDryRun_NeverTouchesWorkspace: a dry run on a fresh root must not create
// anything at all — no files, not even the .scaffold state directory.
func TestDryRun_NeverTouchesWorkspace(t *testing.T) {
	root := t.TempDir()
	p := samplePlan()
	dumpPlan(t, "dryrun-untouched", p)

	before := dirSnapshot(t, root)
	e := New(root, Options{Workers: 3})
	dry, err := e.DryRun(context.Background(), p)
	dumpReport(t, "dryrun-untouched/dry", dry)
	if err != nil || !dry.Success {
		t.Fatalf("dry run failed: %v", err)
	}
	if !dry.DryRun {
		t.Fatal("report must be marked as dry run")
	}
	for id, s := range statuses(dry) {
		if s != StatusCreated {
			t.Fatalf("fresh root: expected created for %s, got %s", id, s)
		}
	}
	after := dirSnapshot(t, root)
	if len(before) != 0 || len(after) != 0 {
		t.Fatalf("dry run touched the workspace: before=%v after=%v", before, after)
	}
	if _, statErr := os.Stat(filepath.Join(root, ManifestDir)); !os.IsNotExist(statErr) {
		t.Fatal("dry run must not even create the state directory")
	}
}

// TestDryRun_MatchesRealRunPerTarget walks a workspace through its whole
// lifecycle and asserts the dry-run conclusion equals the real-run outcome
// at every step: create, verify, recreate (plan change), modified (user
// edit), preexisting (foreign file).
func TestDryRun_MatchesRealRunPerTarget(t *testing.T) {
	root := t.TempDir()
	e := New(root, Options{Workers: 3})
	p := samplePlan()
	dumpPlan(t, "dryrun-parity", p)
	ctx := context.Background()

	step := func(tag string) {
		t.Helper()
		dry, derr := e.DryRun(ctx, p)
		real, rerr := e.Run(ctx, p)
		dumpReport(t, tag+"/dry", dry)
		dumpReport(t, tag+"/real", real)
		if (derr == nil) != (rerr == nil) {
			t.Fatalf("[%s] error divergence: dry=%v real=%v", tag, derr, rerr)
		}
		assertSameStatuses(t, tag, dry, real)
	}

	step("fresh-create") // all created
	step("idempotent")   // all verified

	// Plan changes output for one untouched target -> recreated.
	p.Targets["readme"].Template = "# {{.params.module}} v2\n"
	step("plan-changed")

	// User edits a managed file -> modified, left untouched on both sides.
	writeFile(t, root, "go.mod", "module example.com/HACKED\n")
	step("user-modified")
	if got := readFile(t, root, "go.mod"); got != "module example.com/HACKED\n" {
		t.Fatal("real run must leave a user-modified file untouched")
	}

	// A foreign file appears at a path a new target wants -> preexisting.
	writeFile(t, root, "foreign.txt", "user content\n")
	p.Targets["foreign"] = &plan.Target{Path: "foreign.txt", Template: "generated\n"}
	step("preexisting")
	if got := readFile(t, root, "foreign.txt"); got != "user content\n" {
		t.Fatal("real run must leave a preexisting file untouched")
	}
}

// TestDryRun_FailureAlsoMatches: when the plan contains a failing target,
// the dry run predicts the same per-target failure/skip pattern and still
// writes nothing.
func TestDryRun_FailureAlsoMatches(t *testing.T) {
	root := t.TempDir()
	e := New(root, Options{Workers: 2})
	p := &plan.Plan{Targets: map[string]*plan.Target{
		"a":   {Path: "a.txt", Template: "A\n"},
		"bad": {Path: "bad.txt", Template: "x", Fail: true},
		"c":   {Path: "c.txt", Template: "C\n", Deps: []string{"bad"}},
	}}
	dumpPlan(t, "dryrun-failure", p)
	ctx := context.Background()

	dry, derr := e.DryRun(ctx, p)
	dumpReport(t, "dryrun-failure/dry", dry)
	if derr == nil {
		t.Fatal("dry run must surface the predicted failure")
	}
	if len(dirSnapshot(t, root)) != 0 {
		t.Fatal("failing dry run must not write anything")
	}

	real, rerr := e.Run(ctx, p)
	dumpReport(t, "dryrun-failure/real", real)
	if rerr == nil {
		t.Fatal("real run must fail as predicted")
	}
	assertSameStatuses(t, "dryrun-failure", dry, real)
	if got := statuses(real); got["a"] != StatusCreated || got["bad"] != StatusFailed || got["c"] != StatusSkipped {
		t.Fatalf("unexpected real-run pattern: %v", got)
	}
	// The failed real run rolled back: workspace is empty again, so a
	// repeated dry run still predicts "created" for a.
	dry2, _ := e.DryRun(ctx, p)
	if statuses(dry2)["a"] != StatusCreated {
		t.Fatalf("after rollback, dry run must predict created again, got %s", statuses(dry2)["a"])
	}
}

// TestDryRun_DoesNotConsumeAttempts: dry runs are pure observation; the
// attempt counter only advances on real processing.
func TestDryRun_DoesNotConsumeAttempts(t *testing.T) {
	root := t.TempDir()
	e := New(root, Options{Workers: 1})
	p := samplePlan()
	ctx := context.Background()
	if _, err := e.DryRun(ctx, p); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DryRun(ctx, p); err != nil {
		t.Fatal(err)
	}
	real, err := e.Run(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	for _, res := range real.Results {
		if res.Attempt != 1 {
			t.Fatalf("target %s: dry runs must not consume attempts, got attempt=%d", res.Target, res.Attempt)
		}
	}
}

// TestDryRun_ReportIsExplainable: the dry-run report carries order, statuses
// and reasons in JSON so CI can diff it against a real run.
func TestDryRun_ReportIsExplainable(t *testing.T) {
	root := t.TempDir()
	p := samplePlan()
	dry, err := New(root, Options{Workers: 2}).DryRun(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.MarshalIndent(dry, "", "  ")
	t.Logf("[dryrun-report] REPORT:\n%s", data)
	if !json.Valid(data) {
		t.Fatal("report must serialize to valid JSON")
	}
}
