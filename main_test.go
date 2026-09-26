package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// buildBinary compiles the CLI once for the package's tests.
func buildBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "scaffold-engine")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build cli: %v\n%s", err, out)
	}
	return bin
}

func seedWorkspace(t *testing.T, dir string) {
	t.Helper()
	tmpl := filepath.Join(dir, "templates")
	if err := os.MkdirAll(tmpl, 0o755); err != nil {
		t.Fatal(err)
	}
	plan := `{
	  "name": "svc", "version": 1,
	  "params": [{"name": "S", "required": true}],
	  "targets": [
	    {"name":"readme","template":"templates/r.tpl","output_path":"README.md","params":{"S":"x"}},
	    {"name":"main","template":"templates/m.tpl","output_path":"main.go","depends_on":["readme"],"params":{"S":"x"}}
	  ]
	}`
	os.WriteFile(filepath.Join(dir, "plan.json"), []byte(plan), 0o644)
	os.WriteFile(filepath.Join(tmpl, "r.tpl"), []byte("# {{.S}}\n"), 0o644)
	os.WriteFile(filepath.Join(tmpl, "m.tpl"), []byte("// {{.S}}\n"), 0o644)
}

func runCLI(t *testing.T, bin, dir string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			t.Fatalf("invoke cli: %v", err)
		}
	}
	return string(out), code
}

func TestCLIRunIdempotent(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	seedWorkspace(t, dir)

	out, code := runCLI(t, bin, dir, "run", "--plan", filepath.Join(dir, "plan.json"), "--root", dir)
	if code != 0 {
		t.Fatalf("first run failed: %s", out)
	}
	if !strings.Contains(out, "write") || !strings.Contains(out, "[readme main]") {
		t.Fatalf("unexpected run output:\n%s", out)
	}
	body, _ := os.ReadFile(filepath.Join(dir, "README.md"))
	if string(body) != "# x\n" {
		t.Fatalf("content = %q", body)
	}

	out2, code := runCLI(t, bin, dir, "run", "--plan", filepath.Join(dir, "plan.json"), "--root", dir)
	if code != 0 {
		t.Fatalf("second run failed: %s", out2)
	}
	if strings.Contains(out2, "write") {
		t.Fatalf("second run must not write anything:\n%s", out2)
	}
	if !strings.Contains(out2, "up_to_date") {
		t.Fatalf("expected up_to_date:\n%s", out2)
	}
}

func TestCLICycleRejected(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	seedWorkspace(t, dir)
	cycle := `{"name":"c","version":1,"targets":[
	  {"name":"a","template":"templates/r.tpl","output_path":"a.txt","depends_on":["b"]},
	  {"name":"b","template":"templates/r.tpl","output_path":"b.txt","depends_on":["a"]}]}`
	os.WriteFile(filepath.Join(dir, "cycle.json"), []byte(cycle), 0o644)
	out, code := runCLI(t, bin, dir, "run", "--plan", filepath.Join(dir, "cycle.json"), "--root", dir)
	if code == 0 {
		t.Fatal("cycle must fail")
	}
	if !strings.Contains(out, "cyclic_dependency") {
		t.Fatalf("expected categorized cycle error:\n%s", out)
	}
}

func TestCLIUserModifiedAndForce(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	seedWorkspace(t, dir)
	if _, code := runCLI(t, bin, dir, "run", "--plan", filepath.Join(dir, "plan.json"), "--root", dir); code != 0 {
		t.Fatal("initial run")
	}
	// Tamper with an engine-owned file.
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("EDITED\n"), 0o644)
	out, code := runCLI(t, bin, dir, "run", "--plan", filepath.Join(dir, "plan.json"), "--root", dir)
	if code == 0 || !strings.Contains(out, "user_modified") {
		t.Fatalf("expected user_modified refusal:\n%s", out)
	}
	body, _ := os.ReadFile(filepath.Join(dir, "main.go"))
	if string(body) != "EDITED\n" {
		t.Fatal("tampered content must be preserved on refusal")
	}
	if _, code := runCLI(t, bin, dir, "run", "--plan", filepath.Join(dir, "plan.json"), "--root", dir, "--force"); code != 0 {
		t.Fatal("force run should succeed")
	}
	body, _ = os.ReadFile(filepath.Join(dir, "main.go"))
	if string(body) != "// x\n" {
		t.Fatalf("force must regenerate: %q", body)
	}
}

func TestCLIDryRunWritesNothing(t *testing.T) {
	bin := buildBinary(t)
	dir := t.TempDir()
	seedWorkspace(t, dir)
	if _, code := runCLI(t, bin, dir, "run", "--plan", filepath.Join(dir, "plan.json"), "--root", dir, "--dry-run"); code != 0 {
		t.Fatal("dry-run")
	}
	if _, err := os.Stat(filepath.Join(dir, "README.md")); !os.IsNotExist(err) {
		t.Fatal("dry-run must not create outputs")
	}
	if _, err := os.Stat(filepath.Join(dir, ".scaffold", "manifest.json")); !os.IsNotExist(err) {
		t.Fatal("dry-run must not create a manifest")
	}
}
