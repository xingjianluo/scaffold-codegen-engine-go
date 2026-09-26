// Package render renders target templates with parameters. It uses Go's
// text/template with a missing-key=error option and pre-parses every template
// AST to report which parameters are missing BEFORE any file is written.
package render

import (
	"bytes"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/template"
	"text/template/parse"

	"github.com/xingjianluo/scaffold-codegen-engine-go/plan"
)

// Renderer renders templates for targets of one plan.
type Renderer struct {
	p        *plan.Plan
	parsed   map[string]*template.Template // keyed by clean template path
	defaults map[string]string
	required map[string]bool
}

// NewRenderer creates a Renderer for the given plan. All template files are
// read and parsed up front so a broken template rejects the run before any
// output is produced.
func NewRenderer(p *plan.Plan) (*Renderer, error) {
	r := &Renderer{
		p:        p,
		parsed:   make(map[string]*template.Template),
		defaults: make(map[string]string),
		required: make(map[string]bool),
	}
	for _, ps := range p.Params {
		r.defaults[ps.Name] = ps.Default
		if ps.Required {
			r.required[ps.Name] = true
		}
	}
	ve := &plan.ValidationError{}
	seen := make(map[string]bool)
	for _, t := range p.Targets {
		key := strings.ReplaceAll(t.Template, "/", string(os.PathSeparator))
		if seen[key] {
			continue
		}
		seen[key] = true
		path := p.ResolveTemplate(&t)
		raw, err := os.ReadFile(path)
		if err != nil {
			ve.Add(plan.KindMissingTemplate, t.Name, "cannot read template "+t.Template+": "+err.Error())
			continue
		}
		tpl, err := template.New(t.Template).Option("missingkey=error").Parse(string(raw))
		if err != nil {
			ve.Add(plan.KindTemplateError, t.Name, "template parse error in "+t.Template+": "+err.Error())
			continue
		}
		r.parsed[key] = tpl
	}
	if len(ve.Issues) > 0 {
		return nil, ve
	}
	return r, nil
}

// values merges plan defaults with the target's overrides. Parameters with
// no default are left absent so the template's missingkey=error option fires
// on an unset reference.
func (r *Renderer) values(t plan.Target) map[string]string {
	v := make(map[string]string, len(r.defaults)+len(t.Params))
	for k, d := range r.defaults {
		if d != "" {
			v[k] = d
		}
	}
	for k, val := range t.Params {
		v[k] = val
	}
	return v
}

// MissingParams reports parameter names referenced by the target's template
// that have no effective non-empty value. The template AST is inspected,
// which also finds variables used inside conditionals.
func (r *Renderer) MissingParams(t plan.Target) []string {
	key := strings.ReplaceAll(t.Template, "/", string(os.PathSeparator))
	tpl := r.parsed[key]
	if tpl == nil {
		return nil
	}
	values := r.values(t)
	refs := collectFieldRefs(tpl.Tree)
	missing := []string{}
	for _, name := range refs {
		if strings.TrimSpace(values[name]) == "" {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	return missing
}

// CheckParams validates every target's template references against the
// effective parameter values and returns a ValidationError over all targets.
func (r *Renderer) CheckParams() error {
	ve := &plan.ValidationError{}
	for _, t := range r.p.Targets {
		// Only names that were actually declared count; a template reference
		// to an undeclared name is a missing parameter here.
		declared := make(map[string]bool)
		for _, ps := range r.p.Params {
			declared[ps.Name] = true
		}
		for _, name := range r.MissingParams(t) {
			if !declared[name] {
				ve.Add(plan.KindUnknownParam, t.Name,
					"template references undeclared parameter: "+name)
			} else {
				ve.Add(plan.KindMissingParam, t.Name,
					"template parameter has no value or default: "+name)
			}
		}
	}
	if len(ve.Issues) > 0 {
		return ve
	}
	return nil
}

// Render produces the deterministic bytes for a target.
func (r *Renderer) Render(t plan.Target) ([]byte, error) {
	key := strings.ReplaceAll(t.Template, "/", string(os.PathSeparator))
	tpl := r.parsed[key]
	if tpl == nil {
		return nil, &plan.ValidationError{Issues: []plan.Issue{{
			Kind: plan.KindMissingTemplate, Target: t.Name,
			Detail: "template was not parsed: " + t.Template,
		}}}
	}
	// Emit params in a fixed structure: top-level field access .Name.
	data := r.values(t)
	var buf bytes.Buffer
	if err := tpl.Execute(&buf, data); err != nil {
		return nil, &plan.ValidationError{Issues: []plan.Issue{{
			Kind: plan.KindTemplateError, Target: t.Name,
			Detail: fmt.Sprintf("render failed: %v", err),
		}}}
	}
	return buf.Bytes(), nil
}

// collectFieldRefs walks a parsed template tree and returns the distinct
// top-level parameter names it references. Two AST shapes are recognised:
//
//   - *parse.FieldNode ".Foo" while the pipeline dot is still the top-level
//     data (i.e. outside a range/with body);
//   - *parse.VariableNode "$.Foo" (ident chain ["$","Foo"]), which refers to
//     top-level data even inside a range/with body.
//
// Bare ".Bar" inside a {{range}}/{{with}} body is skipped because the dot has
// been rebound to the iteration/current value; its else-list keeps the outer
// dot and is therefore traversed with the enclosing context.
func collectFieldRefs(tree *parse.Tree) []string {
	names := make(map[string]struct{})
	var walkNode func(n parse.Node, topDot bool)
	var walkList func(l *parse.ListNode, topDot bool)
	var walkPipe func(p *parse.PipeNode, topDot bool)
	walkList = func(l *parse.ListNode, topDot bool) {
		if l == nil {
			return
		}
		for _, n := range l.Nodes {
			walkNode(n, topDot)
		}
	}
	walkPipe = func(p *parse.PipeNode, topDot bool) {
		if p == nil {
			return
		}
		for _, c := range p.Cmds {
			for _, a := range c.Args {
				walkNode(a, topDot)
			}
		}
	}
	walkNode = func(n parse.Node, topDot bool) {
		switch node := n.(type) {
		case *parse.FieldNode:
			// ".Foo" at top-level data context is a parameter reference.
			if topDot && len(node.Ident) > 0 {
				names[node.Ident[0]] = struct{}{}
			}
		case *parse.VariableNode:
			// "$.Foo" / "$" : ident[0] == "$" means the root data. A plain
			// "$" with no field is not a parameter reference.
			if len(node.Ident) >= 2 && node.Ident[0] == "$" {
				names[node.Ident[1]] = struct{}{}
			}
		case *parse.ChainNode:
			// Chains such as "(.X).Y" or "$v.Y": inspect their inner node
			// with the same context; .Field beyond the first access is a
			// nested map key, not a plan parameter.
			walkNode(node.Node, topDot)
		case *parse.ActionNode:
			walkPipe(node.Pipe, topDot)
		case *parse.IfNode:
			walkPipe(node.Pipe, topDot)
			walkList(node.List, topDot)
			walkList(node.ElseList, topDot)
		case *parse.RangeNode:
			walkPipe(node.Pipe, topDot)
			walkList(node.List, false)      // dot rebound to iteration value
			walkList(node.ElseList, topDot) // else keeps outer dot
		case *parse.WithNode:
			walkPipe(node.Pipe, topDot)
			walkList(node.List, false)
			walkList(node.ElseList, topDot)
		case *parse.TemplateNode:
			walkPipe(node.Pipe, topDot)
		case *parse.ListNode:
			walkList(node, topDot)
		case *parse.TextNode, *parse.BreakNode, *parse.ContinueNode:
			// no sub-nodes
		}
	}
	walkList(tree.Root, true)
	out := make([]string, 0, len(names))
	for n := range names {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
