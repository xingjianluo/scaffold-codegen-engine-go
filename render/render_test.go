package render

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xingjianluo/scaffold-codegen-engine-go/plan"
)

func writeRenderPlan(t *testing.T, templates map[string]string, targetsJSON string) *plan.Plan {
	t.Helper()
	dir := t.TempDir()
	for rel, body := range templates {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(full), 0o755)
		os.WriteFile(full, []byte(body), 0o644)
	}
	pp := filepath.Join(dir, "plan.json")
	os.WriteFile(pp, []byte(targetsJSON), 0o644)
	p, err := plan.Load(pp)
	if err != nil {
		t.Fatalf("plan load: %v", err)
	}
	return p
}

func TestRenderDeterministic(t *testing.T) {
	p := writeRenderPlan(t, map[string]string{"b.tpl": `name={{.P}};top={{$.P}}`},
		`{"name":"p","version":1,"params":[{"name":"P","required":true}],"targets":[
		 {"name":"a","template":"b.tpl","output_path":"a.txt","params":{"P":"demo"}}]}`)
	r, err := NewRenderer(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.CheckParams(); err != nil {
		t.Fatal(err)
	}
	out, err := r.Render(p.Targets[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "name=demo;top=demo" {
		t.Fatalf("got %q", out)
	}
	// Repeated rendering must be byte-identical.
	out2, _ := r.Render(p.Targets[0])
	if string(out2) != string(out) {
		t.Fatal("render not deterministic")
	}
}

func TestMissingParamDetectedFromAST(t *testing.T) {
	// {{.Gone}} used in an if-branch that rendering would not hit for an
	// empty map still must be caught by the AST check.
	p := writeRenderPlan(t, map[string]string{"a.tpl": `{{if .Nope}}x{{else}}{{.AlsoGone}}{{end}}`},
		`{"name":"p","version":1,"params":[],"targets":[
		 {"name":"a","template":"a.tpl","output_path":"a.txt"}]}`)
	r, err := NewRenderer(p)
	if err != nil {
		t.Fatal(err)
	}
	err = r.CheckParams()
	ve, ok := err.(*plan.ValidationError)
	if !ok {
		t.Fatalf("want validation error, got %v", err)
	}
	joined := ""
	for _, is := range ve.Issues {
		joined += is.Target + ":" + is.Detail + "\n"
	}
	if !strings.Contains(joined, "Nope") || !strings.Contains(joined, "AlsoGone") {
		t.Fatalf("missing refs not detected:\n%s", joined)
	}
	t.Logf("missing parameter report:\n%s", joined)
}

func TestRangeDotNotCountedAsParam(t *testing.T) {
	// .Name inside range .Items binds to the loop element; $.P is a param.
	p := writeRenderPlan(t, map[string]string{"a.tpl": `{{range .X}}{{.Name}}{{$.P}}{{end}}`},
		`{"name":"p","version":1,"params":[{"name":"P","required":true}],"targets":[
		 {"name":"a","template":"a.tpl","output_path":"a.txt","params":{"P":"v"}}]}`)
	r, err := NewRenderer(p)
	if err != nil {
		t.Fatal(err)
	}
	// .X and .Name inside range: .X is referenced at top level but not
	// declared -> unknown; .Name must NOT be reported.
	err = r.CheckParams()
	ve, _ := err.(*plan.ValidationError)
	if ve == nil {
		t.Fatal("expected an issue for X")
	}
	for _, is := range ve.Issues {
		if strings.Contains(is.Detail, "Name") {
			t.Fatalf("loop variable mistaken for parameter: %v", is)
		}
	}
}

func TestRenderMissingKeyValue(t *testing.T) {
	p := writeRenderPlan(t, map[string]string{"a.tpl": `{{.Undeclared}}`},
		`{"name":"p","version":1,"params":[{"name":"Undeclared","required":true}],"targets":[
		 {"name":"a","template":"a.tpl","output_path":"a.txt","params":{"Undeclared":"d"}}]}`)
	r, err := NewRenderer(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Render(p.Targets[0]); err != nil {
		t.Fatalf("normal render should succeed: %v", err)
	}
	// Simulate a parameter map without the key at all: missingkey=error must
	// surface a categorized error rather than emit "<no value>".
	empty := p.Targets[0]
	empty.Params = nil
	if _, err := r.Render(empty); err == nil {
		t.Fatal("expected missingkey render error")
	} else if !strings.Contains(err.Error(), "render failed") {
		t.Fatalf("want categorized render error, got %v", err)
	}
}
