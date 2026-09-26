package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/xingjianluo/scaffold-codegen-engine-go/plan"
	"github.com/xingjianluo/scaffold-codegen-engine-go/render"
	"github.com/xingjianluo/scaffold-codegen-engine-go/state"
)

func TestSmokeEndToEnd(t *testing.T) {
	root := t.TempDir()
	// copy smoke testdata into root
	copyDir(t, "../testdata/smoke", root)

	p, err := plan.Load(filepath.Join(root, "plan.json"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	r, err := render.NewRenderer(p)
	if err != nil {
		t.Fatalf("renderer: %v", err)
	}
	if err := r.CheckParams(); err != nil {
		t.Fatalf("checkparams: %v", err)
	}
	st := state.NewStore(root)
	e := New(p, st, r)
	rep, err := e.RunContext(context.Background(), Options{Workers: 2})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	t.Logf("order=%v", rep.Order)
	for _, rr := range rep.Results {
		t.Logf("result %+v", rr)
	}
	body, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatalf("readme: %v", err)
	}
	if string(body) != "# demo\n" {
		t.Fatalf("unexpected readme: %q", body)
	}

	// Second run: idempotent.
	rep2, err := e.RunContext(context.Background(), Options{Workers: 2})
	if err != nil {
		t.Fatalf("run2: %v", err)
	}
	for _, rr := range rep2.Results {
		if rr.Written {
			t.Fatalf("second run rewrote %s", rr.Name)
		}
		if rr.Status != state.StatusUpToDate {
			t.Fatalf("second run status for %s = %s", rr.Name, rr.Status)
		}
	}
}

func copyDir(t *testing.T, src, dst string) {
	t.Helper()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, en := range entries {
		s := filepath.Join(src, en.Name())
		d := filepath.Join(dst, en.Name())
		if en.IsDir() {
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatal(err)
			}
			copyDir(t, s, d)
		} else {
			b, err := os.ReadFile(s)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(d), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(d, b, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
}
