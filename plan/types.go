// Package plan defines the declarative generation plan and its validation
// rules. A plan is fully local: it references only files and templates inside
// the workspace and never requires external services.
//
// Plans are JSON documents (see testdata for examples). Load parses and
// performs every static validation; it returns a *ValidationError whose
// Issues list may contain several distinct rejection reasons at once.
package plan

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ParamSpec declares a single global parameter of the plan.
type ParamSpec struct {
	Name     string `json:"name"`
	Required bool   `json:"required"`
	Default  string `json:"default,omitempty"`
}

// Target is one unit of generation: render Template with Params into
// OutputPath. DependsOn names targets that must complete successfully first.
type Target struct {
	Name       string            `json:"name"`
	Template   string            `json:"template"`
	OutputPath string            `json:"output_path"`
	Params     map[string]string `json:"params,omitempty"`
	DependsOn  []string          `json:"depends_on,omitempty"`
}

// Plan is the whole declarative generation plan.
type Plan struct {
	Name    string      `json:"name"`
	Version int         `json:"version"`
	Params  []ParamSpec `json:"params,omitempty"`
	Targets []Target    `json:"targets"`

	// baseDir is the directory containing the plan file; template paths are
	// resolved relative to it. It is populated by Load.
	baseDir string
}

// BaseDir reports the plan file's directory.
func (p *Plan) BaseDir() string { return p.baseDir }

// ResolveTemplate returns the absolute path of a target's template.
func (p *Plan) ResolveTemplate(t *Target) string {
	return filepath.Join(p.baseDir, filepath.FromSlash(t.Template))
}

// Load reads, parses and statically validates a plan file.
func Load(path string) (*Plan, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, &ValidationError{Issues: []Issue{{
			Kind:   KindParse,
			Detail: "cannot read plan file: " + err.Error(),
		}}}
	}
	var p Plan
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return nil, &ValidationError{Issues: []Issue{{
			Kind:   KindParse,
			Detail: "invalid JSON: " + err.Error(),
		}}}
	}
	abs, err := filepath.Abs(path)
	if err == nil {
		p.baseDir = filepath.Dir(abs)
	} else {
		p.baseDir = filepath.Dir(path)
	}
	if err := p.validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

// validate runs every structural check and collects all issues.
func (p *Plan) validate() error {
	ve := &ValidationError{}

	if strings.TrimSpace(p.Name) == "" {
		ve.Add(KindParse, "", "plan.name must be non-empty")
	}
	if p.Version <= 0 {
		ve.Add(KindParse, "", "plan.version must be a positive integer")
	}

	// Declared global parameters: detect duplicates and build the known set.
	knownParams := make(map[string]bool)
	for _, ps := range p.Params {
		if ps.Name == "" {
			ve.Add(KindInvalidPath, "", "a parameter has an empty name")
			continue
		}
		if knownParams[ps.Name] {
			ve.Add(KindDuplicateName, "", "duplicate parameter declaration: "+ps.Name)
		}
		knownParams[ps.Name] = true
	}

	// Effective parameter values: per-target overrides win over defaults.
	effective := make(map[string]string)
	for _, ps := range p.Params {
		if ps.Default != "" {
			effective[ps.Name] = ps.Default
		}
	}

	if len(p.Targets) == 0 {
		ve.Add(KindEmptyPlan, "", "plan declares no targets")
	}

	names := make(map[string]bool)
	outPaths := make(map[string]string) // clean output path -> first target
	for i := range p.Targets {
		t := &p.Targets[i]
		if strings.TrimSpace(t.Name) == "" {
			ve.Add(KindDuplicateName, "", fmt.Sprintf("target at index %d has an empty name", i))
		} else if names[t.Name] {
			ve.Add(KindDuplicateName, t.Name, "target name is declared more than once")
		} else {
			names[t.Name] = true
		}

		if t.Template == "" {
			ve.Add(KindParse, t.Name, "template must be non-empty")
		}
		if !safeRelPath(t.Template) {
			ve.Add(KindInvalidPath, t.Name, "template path must be a clean relative path without '..': "+t.Template)
		} else if _, err := os.Stat(p.ResolveTemplate(t)); err != nil {
			ve.Add(KindMissingTemplate, t.Name, "template not found: "+t.Template)
		}

		if t.OutputPath == "" {
			ve.Add(KindInvalidPath, t.Name, "output_path must be non-empty")
		} else if !safeRelPath(t.OutputPath) {
			ve.Add(KindInvalidPath, t.Name, "output_path must be a clean relative path without '..': "+t.OutputPath)
		} else {
			clean := filepath.ToSlash(filepath.Clean(t.OutputPath))
			if other, ok := outPaths[clean]; ok {
				ve.Add(KindDuplicateOutput, t.Name,
					"output path "+clean+" is also produced by target "+other)
			} else {
				outPaths[clean] = t.Name
			}
			if strings.HasPrefix(clean, ".scaffold/") || clean == ".scaffold" {
				ve.Add(KindInvalidPath, t.Name, "output_path must not be inside the engine state directory .scaffold/")
			}
		}

		// Per-target parameter names must be declared at plan level.
		pnames := make([]string, 0, len(t.Params))
		for k := range t.Params {
			pnames = append(pnames, k)
		}
		sort.Strings(pnames)
		for _, k := range pnames {
			if !knownParams[k] {
				ve.Add(KindUnknownParam, t.Name, "parameter is not declared in plan.params: "+k)
			}
		}
		// Target override counts as an effective value for required checks.
		for k, v := range t.Params {
			effective[k] = v
		}
	}

	// Dependency references and cycle detection. Duplicate edges are rejected
	// as ambiguous configuration.
	depSet := make(map[string]map[string]bool)
	for i := range p.Targets {
		t := &p.Targets[i]
		seen := make(map[string]bool)
		ds := make(map[string]bool)
		for _, d := range t.DependsOn {
			if d == t.Name {
				ve.Add(KindCycle, t.Name, "target depends on itself: "+d)
			}
			if !names[d] {
				ve.Add(KindUnknownDep, t.Name, "depends_on references unknown target: "+d)
			}
			if seen[d] {
				ve.Add(KindDuplicateName, t.Name, "duplicate dependency on target: "+d)
			}
			seen[d] = true
			ds[d] = true
		}
		depSet[t.Name] = ds
	}
	if cycle := findCycle(p.Targets, depSet); cycle != nil {
		ve.Add(KindCycle, cycle[0], "dependency cycle detected: "+strings.Join(cycle, " -> "))
	}

	// Required global parameters must have an effective non-empty value.
	for _, ps := range p.Params {
		if ps.Required && strings.TrimSpace(effective[ps.Name]) == "" {
			ve.Add(KindMissingParam, "", "required parameter has no value or default: "+ps.Name)
		}
	}

	if len(ve.Issues) > 0 {
		return ve
	}
	return nil
}

// findCycle returns one cycle as an ordered list of target names, or nil.
func findCycle(targets []Target, deps map[string]map[string]bool) []string {
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := make(map[string]int)
	var stack []string
	var dfs func(n string) []string
	dfs = func(n string) []string {
		color[n] = gray
		stack = append(stack, n)
		// deterministic edge traversal
		ds := make([]string, 0, len(deps[n]))
		for d := range deps[n] {
			ds = append(ds, d)
		}
		sort.Strings(ds)
		for _, d := range ds {
			if color[d] == white {
				if c := dfs(d); c != nil {
					return c
				}
			} else if color[d] == gray {
				// extract the cycle from the stack
				out := []string{}
				inCycle := false
				for _, s := range stack {
					if s == d {
						inCycle = true
					}
					if inCycle {
						out = append(out, s)
					}
				}
				out = append(out, d)
				return out
			}
		}
		stack = stack[:len(stack)-1]
		color[n] = black
		return nil
	}
	ns := make([]string, 0, len(targets))
	for _, t := range targets {
		ns = append(ns, t.Name)
	}
	sort.Strings(ns)
	for _, n := range ns {
		if color[n] == white {
			if c := dfs(n); c != nil {
				return c
			}
		}
	}
	return nil
}

// safeRelPath reports whether p is a clean relative path that never escapes
// its base (no absolute components, no "..", no empty segments).
func safeRelPath(p string) bool {
	if p == "" {
		return false
	}
	if filepath.IsAbs(p) || filepath.IsAbs(filepath.FromSlash(p)) {
		return false
	}
	for _, seg := range strings.Split(filepath.ToSlash(p), "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}
