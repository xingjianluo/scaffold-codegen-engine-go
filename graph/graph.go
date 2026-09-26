// Package graph builds a dependency graph from a validated plan and produces
// a deterministic execution order. The plan loader already rejects cycles and
// unknown dependencies; this package treats them as defensive errors too.
package graph

import (
	"sort"

	"github.com/xingjianluo/scaffold-codegen-engine-go/plan"
)

// Graph is the validated DAG of targets keyed by target name.
type Graph struct {
	// deps[name] = set of targets name depends on (must run before name).
	deps map[string]map[string]struct{}
	// rdeps[name] = set of targets that depend on name.
	rdeps map[string]map[string]struct{}
	order []string
}

// Build constructs the DAG and computes the deterministic topological order.
// A cycle yields a *plan.ValidationError carrying a KindCycle issue.
func Build(p *plan.Plan) (*Graph, error) {
	g := &Graph{
		deps:  make(map[string]map[string]struct{}),
		rdeps: make(map[string]map[string]struct{}),
	}
	for _, t := range p.Targets {
		g.deps[t.Name] = make(map[string]struct{})
		if _, ok := g.rdeps[t.Name]; !ok {
			g.rdeps[t.Name] = make(map[string]struct{})
		}
	}
	for _, t := range p.Targets {
		for _, d := range t.DependsOn {
			if _, ok := g.deps[d]; !ok {
				return nil, &plan.ValidationError{Issues: []plan.Issue{{
					Kind:   plan.KindUnknownDep,
					Target: t.Name,
					Detail: "depends_on references unknown target: " + d,
				}}}
			}
			g.deps[t.Name][d] = struct{}{}
			g.rdeps[d][t.Name] = struct{}{}
		}
	}

	// Kahn's algorithm with a lexicographically sorted ready set makes the
	// order fully deterministic regardless of target declaration order.
	remaining := make(map[string]int, len(p.Targets))
	ready := []string{}
	for name, ds := range g.deps {
		remaining[name] = len(ds)
		if len(ds) == 0 {
			ready = append(ready, name)
		}
	}
	sort.Strings(ready)

	for len(ready) > 0 {
		n := ready[0]
		ready = ready[1:]
		g.order = append(g.order, n)
		children := make([]string, 0, len(g.rdeps[n]))
		for c := range g.rdeps[n] {
			children = append(children, c)
		}
		sort.Strings(children)
		for _, c := range children {
			remaining[c]--
			if remaining[c] == 0 {
				ready = insertSorted(ready, c)
			}
		}
	}

	if len(g.order) != len(p.Targets) {
		return nil, &plan.ValidationError{Issues: []plan.Issue{{
			Kind:   plan.KindCycle,
			Detail: "dependency cycle prevents scheduling",
		}}}
	}
	return g, nil
}

func insertSorted(s []string, v string) []string {
	i := sort.SearchStrings(s, v)
	s = append(s, "")
	copy(s[i+1:], s[i:])
	s[i] = v
	return s
}

// Order returns target names in dependency order (deterministic).
func (g *Graph) Order() []string {
	out := make([]string, len(g.order))
	copy(out, g.order)
	return out
}

// Dependents returns names of targets that (transitively) depend on name,
// sorted lexicographically.
func (g *Graph) Dependents(name string) []string {
	seen := make(map[string]struct{})
	var walk func(n string)
	walk = func(n string) {
		children := make([]string, 0, len(g.rdeps[n]))
		for c := range g.rdeps[n] {
			children = append(children, c)
		}
		sort.Strings(children)
		for _, c := range children {
			if _, ok := seen[c]; ok {
				continue
			}
			seen[c] = struct{}{}
			walk(c)
		}
	}
	walk(name)
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
