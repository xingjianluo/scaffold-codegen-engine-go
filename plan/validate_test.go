package plan

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writePlan creates a temp workspace with templates and a plan file.
func writePlan(t *testing.T, planJSON string, templates map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, body := range templates {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, "plan.json")
	if err := os.WriteFile(path, []byte(planJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

const validPlan = `{
  "name": "p", "version": 1,
  "params": [{"name": "P", "required": true}],
  "targets": [
    {"name": "a", "template": "a.tpl", "output_path": "out/a.txt", "params": {"P": "v"}},
    {"name": "b", "template": "b.tpl", "output_path": "out/b.txt", "depends_on": ["a"], "params": {"P": "v"}}
  ]
}`

func TestLoadValid(t *testing.T) {
	path := writePlan(t, validPlan, map[string]string{"a.tpl": "x", "b.tpl": "y"})
	p, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(p.Targets) != 2 {
		t.Fatalf("targets = %d", len(p.Targets))
	}
}

func TestRejectCycle(t *testing.T) {
	body := `{
	  "name": "p", "version": 1,
	  "targets": [
	    {"name": "a", "template": "a.tpl", "output_path": "a.txt", "depends_on": ["b"]},
	    {"name": "b", "template": "b.tpl", "output_path": "b.txt", "depends_on": ["c"]},
	    {"name": "c", "template": "c.tpl", "output_path": "c.txt", "depends_on": ["a"]}
	  ]
	}`
	path := writePlan(t, body, map[string]string{"a.tpl": "x", "b.tpl": "x", "c.tpl": "x"})
	_, err := Load(path)
	ve, ok := err.(*ValidationError)
	if !ok {
		t.Fatalf("want ValidationError, got %T %v", err, err)
	}
	if !ve.Has(KindCycle) {
		t.Fatalf("missing cycle issue, got: %v", ve.Issues)
	}
	t.Logf("INPUT PLAN:\n%s", body)
	for _, is := range ve.Issues {
		t.Logf("issue: kind=%s target=%s detail=%s", is.Kind, is.Target, is.Detail)
	}
	if !strings.Contains(ve.Error(), "a -> b -> c -> a") &&
		!strings.Contains(ve.Error(), "cyclic_dependency") {
		t.Fatalf("cycle detail not explainable: %v", ve)
	}
}

func TestRejectSelfCycle(t *testing.T) {
	body := `{"name":"p","version":1,"targets":[
	  {"name":"a","template":"a.tpl","output_path":"a.txt","depends_on":["a"]}]}`
	path := writePlan(t, body, map[string]string{"a.tpl": "x"})
	_, err := Load(path)
	if ve, ok := err.(*ValidationError); !ok || !ve.Has(KindCycle) {
		t.Fatalf("want cycle, got %v", err)
	}
}

func TestRejectMissingRequiredParam(t *testing.T) {
	body := `{
	  "name": "p", "version": 1,
	  "params": [{"name": "Token", "required": true}],
	  "targets": [
	    {"name": "a", "template": "a.tpl", "output_path": "a.txt"}
	  ]
	}`
	path := writePlan(t, body, map[string]string{"a.tpl": "x"})
	_, err := Load(path)
	ve, ok := err.(*ValidationError)
	if !ok || !ve.Has(KindMissingParam) {
		t.Fatalf("want missing parameter, got %v", err)
	}
	t.Logf("issues: %v", ve.Issues)
}

func TestRejectUnknownDependency(t *testing.T) {
	body := `{"name":"p","version":1,"targets":[
	  {"name":"a","template":"a.tpl","output_path":"a.txt","depends_on":["ghost"]}]}`
	path := writePlan(t, body, map[string]string{"a.tpl": "x"})
	_, err := Load(path)
	if ve, ok := err.(*ValidationError); !ok || !ve.Has(KindUnknownDep) {
		t.Fatalf("want unknown dependency, got %v", err)
	}
}

func TestRejectDuplicateNameAndOutputConflict(t *testing.T) {
	body := `{"name":"p","version":1,"targets":[
	  {"name":"a","template":"a.tpl","output_path": "same.txt"},
	  {"name":"a","template":"a.tpl","output_path": "other.txt"},
	  {"name":"b","template":"a.tpl","output_path": "same.txt"}
	]}`
	path := writePlan(t, body, map[string]string{"a.tpl": "x"})
	_, err := Load(path)
	ve, ok := err.(*ValidationError)
	if !ok {
		t.Fatalf("want ValidationError, got %v", err)
	}
	if !ve.Has(KindDuplicateName) {
		t.Errorf("missing duplicate-name issue: %v", ve.Issues)
	}
	if !ve.Has(KindDuplicateOutput) {
		t.Errorf("missing output-conflict issue: %v", ve.Issues)
	}
}

func TestRejectPathEscapeAndEmptyPlan(t *testing.T) {
	body := `{"name":"p","version":1,"targets":[
	  {"name":"a","template":"a.tpl","output_path":"../escape.txt"}
	]}`
	path := writePlan(t, body, map[string]string{"a.tpl": "x"})
	_, err := Load(path)
	if ve, ok := err.(*ValidationError); !ok || !ve.Has(KindInvalidPath) {
		t.Fatalf("want invalid path, got %v", err)
	}

	path2 := writePlan(t, `{"name":"p","version":1,"targets":[]}`, nil)
	_, err = Load(path2)
	if ve, ok := err.(*ValidationError); !ok || !ve.Has(KindEmptyPlan) {
		t.Fatalf("want empty plan, got %v", err)
	}
}

func TestRejectUnknownParamAndMissingTemplate(t *testing.T) {
	body := `{"name":"p","version":1,"targets":[
	  {"name":"a","template":"nope.tpl","output_path":"a.txt","params":{"Undeclared":"x"}}
	]}`
	path := writePlan(t, body, map[string]string{"other.tpl": "x"})
	_, err := Load(path)
	ve, ok := err.(*ValidationError)
	if !ok {
		t.Fatalf("want ValidationError, got %v", err)
	}
	if !ve.Has(KindUnknownParam) || !ve.Has(KindMissingTemplate) {
		t.Fatalf("want unknown param + missing template, got %v", ve.Issues)
	}
}
