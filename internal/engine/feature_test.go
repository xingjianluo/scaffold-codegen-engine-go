package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xingjianluo/scaffold-codegen-engine-go/internal/plan"
)

// dumpPreview logs the preview order and per-target conclusions in the same
// style as dumpReport: input plan, processing order, final state.
func dumpPreview(t *testing.T, tag string, pv *Preview) {
	t.Helper()
	data, _ := json.MarshalIndent(struct {
		Order   []string        `json:"order"`
		Results []PreviewResult `json:"results"`
		Error   string          `json:"error,omitempty"`
	}{pv.Order, pv.Results, errString(pv.Error)}, "", "  ")
	t.Logf("[%s] PREVIEW PROCESSING ORDER + FINAL STATE:\n%s", tag, data)
}

// snapshotAll maps every file under root to its content, including the
// .scaffold state directory. Used to clone a workspace byte-for-byte.
func snapshotAll(t *testing.T, root string) map[string]string {
	t.Helper()
	return walkTree(t, root, false)
}

// snapshotTree maps every plan-output file under root to its content. The
// .scaffold/ state directory is excluded: state.json is supposed to change on
// every run (it records that run), while the restore guarantee is about the
// generated outputs.
func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	return walkTree(t, root, true)
}

func walkTree(t *testing.T, root string, skipState bool) map[string]string {
	out := map[string]string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if skipState && info.Name() == ManifestDir {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		data, rErr := os.ReadFile(path)
		if rErr != nil {
			return rErr
		}
		out[filepath.ToSlash(rel)] = string(data)
		return nil
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("snapshot %s: %v", root, err)
	}
	return out
}

// copyTree duplicates a workspace (including .scaffold) so preview and run
// can be compared on byte-identical starting states.
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	for rel, body := range snapshotAll(t, src) {
		full := filepath.Join(dst, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestPreview_TouchesNothing: on a workspace that was never run, a preview
// must not create .scaffold/ or any output file.
func TestPreview_TouchesNothing(t *testing.T) {
	root := t.TempDir()
	p := samplePlan()
	dumpPlan(t, "preview-touch", p)

	pv, err := New(root, Options{Workers: 3}).Preview(context.Background(), p)
	dumpPreview(t, "preview-touch", pv)
	if err != nil {
		t.Fatalf("preview failed: %v", err)
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Fatalf("preview created files: %v", entries)
	}
	// Invalid plan previews must also touch nothing.
	bad := &plan.Plan{Targets: map[string]*plan.Target{
		"a": {Path: "a", Deps: []string{"b"}},
		"b": {Path: "b", Deps: []string{"a"}},
	}}
	_, err = New(root, Options{}).Preview(context.Background(), bad)
	var ce *ClassifiedErr
	if !errors.As(err, &ce) || ce.Kind != KindInvalidPlan {
		t.Fatalf("expected invalid_plan, got %v", err)
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Fatalf("invalid preview created files: %v", entries)
	}
}

// TestPreview_ConclusionsMatchRealRun is the headline guarantee: per-target
// preview statuses equal the statuses of a real run started from an identical
// workspace, across created/verified/recreated/modified/preexisting and the
// failed/skipped propagation cases.
func TestPreview_ConclusionsMatchRealRun(t *testing.T) {
	cases := []struct {
		name  string
		plan  func() *plan.Plan
		setup func(t *testing.T, root string, e *Engine, p *plan.Plan)
	}{
		{
			name: "fresh-root-created",
			plan: samplePlan,
		},
		{
			name: "repeat-verified",
			plan: samplePlan,
			setup: func(t *testing.T, root string, e *Engine, p *plan.Plan) {
				if _, err := e.Run(context.Background(), p); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "plan-change-recreated",
			plan: func() *plan.Plan {
				p := samplePlan()
				p.Targets["readme"].Template = "# {{.params.module}} v2\n"
				return p
			},
			setup: func(t *testing.T, root string, e *Engine, _ *plan.Plan) {
				if _, err := e.Run(context.Background(), samplePlan()); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "user-modified-kept",
			plan: samplePlan,
			setup: func(t *testing.T, root string, e *Engine, p *plan.Plan) {
				if _, err := e.Run(context.Background(), p); err != nil {
					t.Fatal(err)
				}
				writeFile(t, root, "go.mod", "module example.com/HACKED\n")
			},
		},
		{
			name: "preexisting-untracked",
			plan: func() *plan.Plan {
				return &plan.Plan{Name: "p2", Targets: map[string]*plan.Target{
					"foreign": {Path: "foreign.txt", Template: "generated\n"},
				}}
			},
			setup: func(t *testing.T, root string, e *Engine, _ *plan.Plan) {
				if _, err := e.Run(context.Background(), samplePlan()); err != nil {
					t.Fatal(err)
				}
				writeFile(t, root, "foreign.txt", "user content\n")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			p := tc.plan()
			dumpPlan(t, "preview-match/"+tc.name, p)
			if tc.setup != nil {
				tc.setup(t, base, New(base, Options{Workers: 2}), p)
			}
			before := snapshotTree(t, base)

			previewRoot := filepath.Join(t.TempDir(), "pv")
			runRoot := filepath.Join(t.TempDir(), "run")
			copyTree(t, base, previewRoot)
			copyTree(t, base, runRoot)

			pv, err := New(previewRoot, Options{Workers: 4}).Preview(context.Background(), p)
			dumpPreview(t, "preview-match/"+tc.name, pv)
			if err != nil {
				t.Fatalf("preview: %v", err)
			}
			rep, rerr := New(runRoot, Options{Workers: 4}).Run(context.Background(), p)
			dumpReport(t, "preview-match/"+tc.name, rep)
			if rerr != nil || !rep.Success {
				t.Fatalf("run: %v", rerr)
			}
			assertSameConclusions(t, pv, rep)

			// Preview must not have changed its copy.
			if got := snapshotTree(t, previewRoot); !treesEqual(before, got) {
				// base == previewRoot contents (both copied before preview);
				// compare against a fresh copy instead for clarity.
				fresh := filepath.Join(t.TempDir(), "fresh")
				copyTree(t, base, fresh)
				if !treesEqual(snapshotTree(t, fresh), got) {
					t.Fatal("preview mutated its workspace")
				}
			}
		})
	}

	// Failure propagation: preview failed/skipped must match the real run.
	t.Run("render-failure-and-skip", func(t *testing.T) {
		p := &plan.Plan{
			Name:   "pfail",
			Params: map[string]string{"module": "example.com/demo"},
			Targets: map[string]*plan.Target{
				"a":   {Path: "a.txt", Template: "A\n"},
				"bad": {Path: "bad.txt", Template: "{{.params.missing_value}}"},
				"c":   {Path: "c.txt", Template: "C {{deps \"bad\"}}", Deps: []string{"bad"}},
				"d":   {Path: "d.txt", Template: "D {{deps \"c\"}}", Deps: []string{"c"}},
			},
		}
		dumpPlan(t, "preview-match/fail", p)
		pvRoot, runRoot := t.TempDir(), t.TempDir()
		pv, perr := New(pvRoot, Options{Workers: 3}).Preview(context.Background(), p)
		dumpPreview(t, "preview-match/fail", pv)
		if perr == nil {
			t.Fatal("preview should report the render failure")
		}
		rep, rerr := New(runRoot, Options{Workers: 3}).Run(context.Background(), p)
		dumpReport(t, "preview-match/fail", rep)
		if rerr == nil {
			t.Fatal("run should fail")
		}
		assertSameConclusions(t, pv, rep)
	})
}

func assertSameConclusions(t *testing.T, pv *Preview, rep *Report) {
	t.Helper()
	if strings.Join(pv.Order, ",") != strings.Join(rep.Order, ",") {
		t.Fatalf("order differs: preview=%v run=%v", pv.Order, rep.Order)
	}
	if len(pv.Results) != len(rep.Results) {
		t.Fatalf("result count differs: %d vs %d", len(pv.Results), len(rep.Results))
	}
	for i := range pv.Results {
		a, b := pv.Results[i], rep.Results[i]
		if a.Target != b.Target || a.Path != b.Path {
			t.Fatalf("row %d identity differs: %+v vs %+v", i, a, b)
		}
		if a.Status != b.Status {
			t.Fatalf("target %s: preview says %s but run did %s (preview: %s)",
				a.Target, a.Status, b.Status, a.Reason)
		}
		if a.SHA256 != b.SHA256 {
			t.Fatalf("target %s: digest differs preview=%s run=%s", a.Target, a.SHA256, b.SHA256)
		}
	}
}

func treesEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// TestRun_RestoresOverwrittenFilesOnFailure: a failing run that recreated
// committed files AND created new ones must restore the workspace to exactly
// the pre-run bytes.
func TestRun_RestoresOverwrittenFilesOnFailure(t *testing.T) {
	root := t.TempDir()
	e := New(root, Options{Workers: 1})
	base := samplePlan()
	dumpPlan(t, "restore-overwrite/base", base)
	if _, err := e.Run(context.Background(), base); err != nil {
		t.Fatal(err)
	}
	pre := snapshotTree(t, root)

	// New plan: rewrites main.go, creates new.txt and a nested new file, and
	// fails rendering "bad".
	p := &plan.Plan{
		Name:   "overwrite-fail",
		Params: map[string]string{"module": "example.com/demo"},
		Targets: map[string]*plan.Target{
			"go-mod": {Path: "go.mod", Template: "module {{.params.module}}\n\ngo 1.25\n"},
			"main":   {Path: "main.go", Template: "BRAND NEW CONTENT {{deps \"go-mod\"}}", Deps: []string{"go-mod"}},
			"new":    {Path: "dir/new.txt", Template: "new\n"},
			"bad":    {Path: "bad.txt", Template: "{{.params.missing_value}}"},
		},
	}
	dumpPlan(t, "restore-overwrite/failing", p)
	rep, err := e.Run(context.Background(), p)
	dumpReport(t, "restore-overwrite/failing", rep)
	if err == nil {
		t.Fatal("expected failure")
	}
	post := snapshotTree(t, root)
	if !treesEqual(pre, post) {
		for k, v := range pre {
			if post[k] != v {
				t.Errorf("path %s not restored:\n pre=%q\npost=%q", k, v, post[k])
			}
		}
		for k := range post {
			if _, ok := pre[k]; !ok {
				t.Errorf("failure left new path %s behind", k)
			}
		}
	}
	// No backup directory may remain after a clean restore.
	if items, _ := os.ReadDir(filepath.Join(root, ManifestDir, BackupDirName)); len(items) != 0 {
		t.Fatalf("backup dir not cleaned: %v", items)
	}

	// A rerun of the good plan converges to a clean success.
	rep2, err2 := e.Run(context.Background(), base)
	dumpReport(t, "restore-overwrite/converged", rep2)
	if err2 != nil || !rep2.Success {
		t.Fatalf("rerun must converge, got %v", err2)
	}
	for _, r := range rep2.Results {
		if r.Status != StatusVerified {
			t.Fatalf("expected verified after restore, %s=%s", r.Target, r.Status)
		}
	}
}

// TestRun_RestoresOnInterruption_LongChain: a long dependency chain where one
// render blocks until cancel, after some targets were created and a committed
// file was recreated, must restore the pre-run state.
func TestRun_RestoresOnInterruption_LongChain(t *testing.T) {
	root := t.TempDir()
	e := New(root, Options{Workers: 2})
	if _, err := e.Run(context.Background(), samplePlan()); err != nil {
		t.Fatal(err)
	}
	pre := snapshotTree(t, root)

	block := make(chan struct{})
	oldHook := renderHook
	renderHook = func(tg *plan.Target, body string, err error) (string, error) {
		if tg.ID == "t5" {
			<-block
		}
		return body, err
	}
	defer func() { renderHook = oldHook }()

	p := &plan.Plan{
		Name:    "chain",
		Params:  map[string]string{"module": "example.com/demo"},
		Targets: map[string]*plan.Target{},
	}
	// t0 reuses go.mod path (will be recreated before t5 blocks); t1..t7 a
	// linear chain, t5 blocks.
	p.Targets["t0"] = &plan.Target{Path: "go.mod", Template: "OVERWRITTEN {{.params.module}}\n"}
	var prev string
	for i := 1; i <= 7; i++ {
		id := fmt.Sprintf("t%d", i)
		tg := &plan.Target{Path: fmt.Sprintf("chain/%d.txt", i), Template: id}
		if prev != "" {
			tg.Deps = []string{prev}
			tg.Template = id + " after " + prev
		}
		if i == 6 {
			tg.Deps = []string{"t5"}
		}
		p.Targets[id] = tg
		prev = id
	}
	dumpPlan(t, "interrupt-chain", p)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	repCh := make(chan *Report, 1)
	go func() {
		r, err := e.Run(ctx, p)
		repCh <- r
		errCh <- err
	}()
	time.Sleep(40 * time.Millisecond)
	cancel()
	close(block)
	if err := <-errCh; err == nil {
		t.Fatal("expected interrupted error")
	}
	rep := <-repCh
	dumpReport(t, "interrupt-chain", rep)
	if !treesEqual(pre, snapshotTree(t, root)) {
		t.Fatal("workspace not restored to pre-run state after interruption")
	}

	// Rerun with the hook removed converges.
	renderHook = oldHook
	rep2, err := New(root, Options{Workers: 4}).Run(context.Background(), p)
	dumpReport(t, "interrupt-chain/converged", rep2)
	if err != nil || !rep2.Success {
		t.Fatalf("post-interrupt rerun must converge: %v", err)
	}
}

// TestStatus_DistinguishesDeletedFromModified is the required distinction.
func TestStatus_DistinguishesDeletedFromModified(t *testing.T) {
	root := t.TempDir()
	e := New(root, Options{Workers: 2})
	p := samplePlan()
	dumpPlan(t, "status-dist", p)
	if _, err := e.Run(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "go.mod", "module example.com/HACKED\n")
	if err := os.Remove(filepath.Join(root, "README.md")); err != nil {
		t.Fatal(err)
	}

	st, err := e.Status(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string]TargetStatus{}
	for _, row := range st.Targets {
		rows[row.Target] = row
		t.Logf("[status-dist] %-8s lastRun=%-11s step=%-9s disk=%s", row.Target, row.LastRun, row.Step, row.Disk)
	}
	if rows["go-mod"].Disk != FileUserModified {
		t.Fatalf("go.mod must be user_modified, got %s", rows["go-mod"].Disk)
	}
	if rows["readme"].Disk != FileDeleted {
		t.Fatalf("readme must be deleted, got %s", rows["readme"].Disk)
	}
	if rows["main"].Disk != FileInSync {
		t.Fatalf("main must be in_sync, got %s", rows["main"].Disk)
	}
	if st.Consistent || st.MatchesPlan {
		t.Fatal("workspace with edits/deletions cannot be consistent")
	}
	if st.Phase != PhaseSucceeded {
		t.Fatalf("phase must be succeeded, got %s", st.Phase)
	}
}

// TestStatus_PlanChangeShowsPlanDrift: untouched disk but a changed plan is
// plan_changed (still consistent with the last run, not matching the plan).
func TestStatus_PlanChangeShowsPlanDrift(t *testing.T) {
	root := t.TempDir()
	e := New(root, Options{})
	p := samplePlan()
	if _, err := e.Run(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	p2 := samplePlan()
	p2.Targets["readme"].Template = "# {{.params.module}} FUTURE\n"
	st, _ := e.Status(context.Background(), p2)
	rows := map[string]TargetStatus{}
	for _, row := range st.Targets {
		rows[row.Target] = row
	}
	if rows["readme"].Disk != FilePlanDrift {
		t.Fatalf("expected plan_changed, got %s (%s)", rows["readme"].Disk, rows["readme"].Reason)
	}
	if !st.Consistent {
		t.Fatal("plan drift must still be consistent with the last committed run")
	}
	if st.MatchesPlan {
		t.Fatal("plan drift must not match the current plan")
	}
}

// TestStatus_PhaseAfterFailureThenConvergence: Status must show where the last
// run stopped, and a fixed-plan rerun must converge.
func TestStatus_PhaseAfterFailureThenConvergence(t *testing.T) {
	root := t.TempDir()
	e := New(root, Options{Workers: 3})
	p := &plan.Plan{Targets: map[string]*plan.Target{
		"a":   {Path: "a.txt", Template: "A\n"},
		"bad": {Path: "bad.txt", Template: "{{.params.missing}}"},
	}}
	dumpPlan(t, "status-phase", p)
	rep, err := e.Run(context.Background(), p)
	dumpReport(t, "status-phase/failed", rep)
	if err == nil {
		t.Fatal("expected failure")
	}
	st, _ := e.Status(context.Background(), p)
	if st.Phase != PhaseFailed || st.Stale {
		t.Fatalf("expected phase=failed stale=false, got phase=%s stale=%v", st.Phase, st.Stale)
	}
	rows := map[string]TargetStatus{}
	for _, row := range st.Targets {
		rows[row.Target] = row
	}
	if rows["bad"].LastRun != StatusFailed || rows["bad"].Step != StepTerminal {
		t.Fatalf("bad must show failed/terminal: %+v", rows["bad"])
	}
	if rows["a"].LastRun != StatusCreated {
		t.Fatalf("a must show created from the failed run record, got %s", rows["a"].LastRun)
	}

	p.Targets["bad"].Template = "ok now\n"
	rep2, err := e.Run(context.Background(), p)
	dumpReport(t, "status-phase/converged", rep2)
	if err != nil || !rep2.Success {
		t.Fatalf("convergence rerun failed: %v", err)
	}
	st2, _ := e.Status(context.Background(), p)
	if st2.Phase != PhaseSucceeded {
		t.Fatalf("expected succeeded, got %s", st2.Phase)
	}
	if !st2.Consistent || !st2.MatchesPlan {
		t.Fatalf("converged workspace must be consistent: %+v", st2)
	}
}

// TestRun_CrossProcessLock_BusyAndQueued: a holder in another process gets an
// immediate workspace_busy; with WaitLock the run queues and proceeds once the
// holder exits.
func TestRun_CrossProcessLock_BusyAndQueued(t *testing.T) {
	if os.Getenv("SCAFFOLD_LOCK_HELPER") == "1" {
		lockHelperMain()
		return
	}
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("helper subprocess unsupported")
	}

	root := t.TempDir()
	ready := filepath.Join(root, "helper-ready")
	release := filepath.Join(root, "helper-release")
	cmd := exec.Command(os.Args[0], "-test.run=^TestRun_CrossProcessLock_BusyAndQueued$")
	cmd.Env = append(os.Environ(),
		"SCAFFOLD_LOCK_HELPER=1",
		"SCAFFOLD_LOCK_ROOT="+root,
		"SCAFFOLD_LOCK_READY="+ready,
		"SCAFFOLD_LOCK_RELEASE="+release,
	)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = os.WriteFile(release, []byte("x"), 0o644)
		_ = cmd.Wait()
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(ready); err != nil {
		t.Fatal("helper never acquired the lock")
	}

	p := samplePlan()
	_, err := New(root, Options{}).Run(context.Background(), p)
	var ce *ClassifiedErr
	if !errors.As(err, &ce) || ce.Kind != KindWorkspaceBusy {
		t.Fatalf("expected workspace_busy, got %v", err)
	}
	if !strings.Contains(ce.Error(), "workspace is locked") {
		t.Fatalf("busy error must explain the occupation: %v", ce)
	}

	// Queued run: release the helper shortly after starting the waiter.
	go func() {
		time.Sleep(300 * time.Millisecond)
		_ = os.WriteFile(release, []byte("x"), 0o644)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rep, err := New(root, Options{WaitLock: true, Workers: 2}).Run(ctx, p)
	if err != nil || !rep.Success {
		t.Fatalf("queued run must succeed after the holder exits: %v", err)
	}
}

// lockHelperMain is the subprocess body that holds the exclusive lock.
func lockHelperMain() {
	root := os.Getenv("SCAFFOLD_LOCK_ROOT")
	ready := os.Getenv("SCAFFOLD_LOCK_READY")
	release := os.Getenv("SCAFFOLD_LOCK_RELEASE")
	holder := lockHolder{Mode: "run", PlanName: "helper", PID: os.Getpid(),
		Host: "helper", StartedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	lock, err := acquireWorkspaceLock(context.Background(), root, true, false, holder)
	if err != nil {
		fmt.Fprintln(os.Stderr, "helper lock:", err)
		os.Exit(1)
	}
	_ = os.WriteFile(ready, []byte("1"), 0o644)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(release); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = lock.release()
}

// TestRun_TwoEngines_BusyWithoutWait: same-process, different Engine instances
// are also coordinated by the file lock.
func TestRun_TwoEngines_BusyWithoutWait(t *testing.T) {
	root := t.TempDir()
	block := make(chan struct{})
	oldHook := renderHook
	renderHook = func(tg *plan.Target, body string, err error) (string, error) {
		if tg.ID == "holder" {
			<-block
		}
		return body, err
	}
	defer func() { renderHook = oldHook }()

	p1 := &plan.Plan{Targets: map[string]*plan.Target{
		"holder": {Path: "holder.txt", Template: "h"},
	}}
	dumpPlan(t, "two-engine", p1)
	holderDone := make(chan struct{})
	go func() {
		defer close(holderDone)
		_, _ = New(root, Options{}).Run(context.Background(), p1)
	}()
	time.Sleep(40 * time.Millisecond)

	p2 := samplePlan()
	_, err := New(root, Options{}).Run(context.Background(), p2)
	var ce *ClassifiedErr
	if !errors.As(err, &ce) || ce.Kind != KindWorkspaceBusy {
		t.Fatalf("second engine must be rejected busy, got %v", err)
	}
	close(block)
	<-holderDone // let the holder finish writing before TempDir cleanup
}

// TestRun_RecoversFromSimulatedCrash: state.json says the last run never
// reached a terminal phase and a backup plus partial files survive; the next
// run must restore from the backup before doing anything.
func TestRun_RecoversFromSimulatedCrash(t *testing.T) {
	root := t.TempDir()
	// A pre-run committed workspace.
	e := New(root, Options{})
	p := samplePlan()
	if _, err := e.Run(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	originalGoMod := readFile(t, root, "go.mod")

	// Simulate a crashed run: backup captured the old go.mod, disk has a
	// half-written overwrite plus a new file, state is "running".
	bk, err := newBackup(root, "run-simulated-crash")
	if err != nil {
		t.Fatal(err)
	}
	if err := bk.captureIfNeeded("go.mod"); err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "go.mod", "HALF WRITTEN CRASH CONTENT\n")
	writeFile(t, root, "go.mod", "HALF WRITTEN CRASH CONTENT\n")
	// In the real flow every write is preceded by captureIfNeeded, so the
	// crashed run's index records this new path as previously absent.
	if err := bk.captureIfNeeded("leftover-new.txt"); err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "leftover-new.txt", "should be deleted\n")
	crashed := &RunState{
		Phase:    PhaseRunning,
		PlanName: "crashed",
		Targets: map[string]*TargetState{
			"go-mod": {Target: "go-mod", Path: "go.mod", Step: StepWritten, Status: StatusRecreated},
		},
	}
	stateDir := filepath.Join(root, ManifestDir)
	if err := crashed.save(stateDir, StateFileName); err != nil {
		t.Fatal(err)
	}

	// The backup must NOT be removed yet (simulating a hard kill).
	st, _ := loadRunState(stateDir, StateFileName)
	if st.Phase != PhaseRunning {
		t.Fatal("sanity")
	}

	rep, err := New(root, Options{Workers: 2}).Run(context.Background(), p)
	dumpReport(t, "crash-recovery", rep)
	if err != nil || !rep.Success {
		t.Fatalf("recovery run must succeed: %v", err)
	}
	if len(rep.Recovered) == 0 {
		t.Fatal("report must record which crashed backup was recovered")
	}
	if got := readFile(t, root, "go.mod"); got != originalGoMod {
		t.Fatalf("crashed overwrite must be restored, got %q", got)
	}
	if _, err := os.Stat(filepath.Join(root, "leftover-new.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("file created by the crashed run must be removed")
	}
	if items, _ := os.ReadDir(filepath.Join(root, ManifestDir, BackupDirName)); len(items) != 0 {
		t.Fatalf("recovered backups must be garbage-collected: %v", items)
	}
}

// TestPreview_WhileRunInProgress_IsBusy: while an exclusive run is active, a
// preview must refuse with workspace_busy rather than race the run's writes.
func TestPreview_WhileRunInProgress_IsBusy(t *testing.T) {
	root := t.TempDir()
	block := make(chan struct{})
	oldHook := renderHook
	renderHook = func(tg *plan.Target, body string, err error) (string, error) {
		if tg.ID == "slow" {
			<-block
		}
		return body, err
	}
	defer func() { renderHook = oldHook }()

	plan1 := &plan.Plan{Targets: map[string]*plan.Target{
		"slow": {Path: "slow.txt", Template: "slow"},
	}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = New(root, Options{}).Run(context.Background(), plan1)
	}()
	time.Sleep(40 * time.Millisecond)

	p := samplePlan()
	_, err := New(root, Options{}).Preview(context.Background(), p)
	var ce *ClassifiedErr
	if !errors.As(err, &ce) || ce.Kind != KindWorkspaceBusy {
		t.Fatalf("preview during a run must be busy, got %v", err)
	}
	// Status is a read-only inspection and is allowed (it reads atomically).
	st, serr := New(root, Options{}).Status(context.Background(), p)
	if serr != nil {
		t.Fatalf("status during a run must still work: %v", serr)
	}
	if st.Phase != PhaseRunning {
		t.Fatalf("expected phase running during active run, got %s", st.Phase)
	}
	close(block)
	<-done
}
