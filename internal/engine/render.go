package engine

import (
	"bytes"
	"fmt"
	"text/template"

	"github.com/xingjianluo/scaffold-codegen-engine-go/internal/plan"
)

// render builds the deterministic data exposed to every target template.
// Params are exposed under lowercase "params" so templates read
// {{.params.KEY}}; dependency bodies are available through the "deps"
// template function: {{deps "target-id"}}.
func renderData(p *plan.Plan) map[string]any {
	return map[string]any{
		"params": p.Params,
	}
}

// renderHook wraps every render result. Production code leaves it nil; tests
// replace it to block or observe rendering without touching the templates.
var renderHook func(t *plan.Target, body string, err error) (string, error)

// render parses and executes one target's template. The template option
// missingkey=error makes references to undefined parameters/data an explicit
// failure instead of silently rendering "<no value>". The provided function
// "deps" returns the rendered body of an already-completed dependency; an
// unknown dependency is a render error rather than empty output.
func render(t *plan.Target, p *plan.Plan, deps map[string]string) (string, error) {
	if t.Fail {
		err := fmt.Errorf("target %q is marked fail: simulated render failure", t.ID)
		return applyHook(t, "", err)
	}
	tmpl, err := template.New(t.ID).Option("missingkey=error").Funcs(template.FuncMap{
		"deps": func(id string) (string, error) {
			body, ok := deps[id]
			if !ok {
				return "", fmt.Errorf("target %q references unknown dependency %q", t.ID, id)
			}
			return body, nil
		},
	}).Parse(t.Template)
	if err != nil {
		return applyHook(t, "", fmt.Errorf("target %q: template parse error: %w", t.ID, err))
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, renderData(p)); err != nil {
		return applyHook(t, "", fmt.Errorf("target %q: template execute error: %w", t.ID, err))
	}
	return applyHook(t, buf.String(), nil)
}

func applyHook(t *plan.Target, body string, err error) (string, error) {
	if renderHook == nil {
		return body, err
	}
	return renderHook(t, body, err)
}
