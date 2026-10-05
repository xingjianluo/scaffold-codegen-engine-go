package plan

import (
	"path"
	"sort"
	"strings"
)

// Validate performs all static checks: structural rules, path conflicts,
// missing dependencies/self-dependencies, required parameters and cycles.
// Every detected problem is returned; validation is never short-circuited, so
// a single run can explain everything that is wrong with a plan.
func Validate(p *Plan) Issues {
	var issues Issues
	if p == nil {
		return Issues{{Code: IssueEmptyID, Message: "plan is nil"}}
	}

	// Deterministic iteration: work with sorted target IDs.
	ids := make([]string, 0, len(p.Targets))
	for id := range p.Targets {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	// path -> target IDs, for path-conflict detection.
	pathOwners := make(map[string][]string)

	for _, id := range ids {
		t := p.Targets[id]
		if t == nil {
			issues = append(issues, Issue{Code: IssueEmptyID, Target: id, Message: "target is null"})
			continue
		}
		// The map key is the authoritative ID; Validate never mutates the
		// plan, so concurrent runs over a shared *Plan are race-free.
		// ExecutionOrder hands out per-call copies carrying the ID.

		if strings.TrimSpace(id) == "" {
			issues = append(issues, Issue{Code: IssueEmptyID, Message: "target id must not be empty"})
		}
		switch {
		case t.Path == "":
			issues = append(issues, Issue{Code: IssueEmptyPath, Target: id, Message: "target " + id + " has an empty path"})
		default:
			if code, msg := checkPath(t.Path); code != "" {
				issues = append(issues, Issue{Code: code, Target: id, Message: msg})
			} else {
				// IDs are visited in sorted order, so owner lists are sorted too.
				pathOwners[t.Path] = append(pathOwners[t.Path], id)
			}
		}

		// Required parameters.
		for _, key := range dedupSorted(t.RequiredParams) {
			v, ok := p.Params[key]
			if !ok || v == "" {
				issues = append(issues, Issue{
					Code:    IssueMissingParam,
					Target:  id,
					Message: "target " + id + " requires parameter \"" + key + "\" which is missing or empty",
				})
			}
		}

		// Dependency references.
		for _, dep := range dedupSorted(t.Deps) {
			switch {
			case dep == id:
				issues = append(issues, Issue{
					Code:    IssueSelfDep,
					Target:  id,
					Message: "target " + id + " depends on itself",
				})
			case p.Targets[dep] == nil:
				issues = append(issues, Issue{
					Code:    IssueMissingDep,
					Target:  id,
					Message: "target " + id + " depends on unknown target \"" + dep + "\"",
				})
			}
		}
	}

	// Path conflicts: a path owned by more than one target is rejected for
	// every owner, regardless of whether the rendered bodies would match.
	confPaths := make([]string, 0, len(pathOwners))
	for fp := range pathOwners {
		if len(uniqueOwners(pathOwners[fp])) > 1 {
			confPaths = append(confPaths, fp)
		}
	}
	sort.Strings(confPaths)
	for _, fp := range confPaths {
		owners := uniqueOwners(pathOwners[fp])
		for _, owner := range owners {
			issues = append(issues, Issue{
				Code:    IssuePathConflict,
				Target:  owner,
				Message: "path \"" + fp + "\" is claimed by targets: " + strings.Join(owners, ", "),
			})
		}
	}

	issues = append(issues, detectCycles(p, ids)...)

	sortIssues(issues)
	return issues
}

// checkPath enforces the output path rules. Paths are slash-separated,
// relative, normalized and must stay inside the run root.
func checkPath(p string) (IssueCode, string) {
	if strings.ContainsRune(p, '\\') {
		return IssuePathNotNormalized, "path \"" + p + "\" must use forward slashes"
	}
	if path.IsAbs(p) {
		return IssuePathAbsolute, "path \"" + p + "\" must be relative"
	}
	if path.Clean(p) != p {
		return IssuePathNotNormalized, "path \"" + p + "\" is not normalized; expected \"" + path.Clean(p) + "\""
	}
	for _, elem := range strings.Split(p, "/") {
		if elem == ".." {
			return IssuePathTraversal, "path \"" + p + "\" escapes the run root"
		}
	}
	return "", ""
}

// detectCycles finds every dependency back edge with a colored DFS. Targets
// are visited in ID order and findings are sorted, so the result is stable.
func detectCycles(p *Plan, ids []string) Issues {
	const white, gray, black = 0, 1, 2
	color := make(map[string]int, len(ids))
	var stack []string
	var issues Issues

	grayIndex := make(map[string]int)

	var visit func(id string)
	visit = func(id string) {
		color[id] = gray
		grayIndex[id] = len(stack)
		stack = append(stack, id)

		t := p.Targets[id]
		if t != nil {
			for _, dep := range dedupSorted(t.Deps) {
				if p.Targets[dep] == nil || dep == id {
					continue // reported separately
				}
				switch color[dep] {
				case white:
					visit(dep)
				case gray:
					cycle := append([]string(nil), stack[grayIndex[dep]:]...)
					cycle = append(cycle, dep)
					issues = append(issues, Issue{
						Code:    IssueCycle,
						Target:  id,
						Message: "dependency cycle: " + strings.Join(cycle, " -> "),
					})
				}
			}
		}

		stack = stack[:len(stack)-1]
		delete(grayIndex, id)
		color[id] = black
	}

	for _, id := range ids {
		if color[id] == white {
			visit(id)
		}
	}
	return issues
}

// ExecutionOrder returns targets in a deterministic topological order.
// Dependencies always precede dependents; among ready targets the smallest ID
// is chosen first, so the order never depends on map iteration.
func ExecutionOrder(p *Plan) ([]*Target, Issues) {
	if issues := Validate(p); len(issues) > 0 {
		return nil, issues
	}

	indeg := make(map[string]int, len(p.Targets))
	children := make(map[string][]string) // dep -> targets waiting on it
	for id, t := range p.Targets {
		indeg[id] = len(dedupSorted(t.Deps))
		for _, dep := range dedupSorted(t.Deps) {
			children[dep] = append(children[dep], id)
		}
	}

	var ready []string
	for id, n := range indeg {
		if n == 0 {
			ready = append(ready, id)
		}
	}
	sort.Strings(ready)

	order := make([]*Target, 0, len(p.Targets))
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		// Hand out a copy carrying the authoritative ID instead of
		// mutating the caller's plan: callers may share one *Plan across
		// concurrent runs.
		cp := *p.Targets[id]
		cp.ID = id
		order = append(order, &cp)

		next := append([]string(nil), children[id]...)
		sort.Strings(next)
		for _, child := range next {
			indeg[child]--
			if indeg[child] == 0 {
				ready = insertSorted(ready, child)
			}
		}
	}

	if len(order) != len(p.Targets) {
		return nil, Validate(p) // defensive: surface the cycle issues
	}
	return order, nil
}

func dedupSorted(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func insertSorted(sorted []string, v string) []string {
	i := sort.SearchStrings(sorted, v)
	sorted = append(sorted, "")
	copy(sorted[i+1:], sorted[i:])
	sorted[i] = v
	return sorted
}

func uniqueOwners(owners []string) []string {
	return dedupSorted(owners)
}

func sortIssues(issues Issues) {
	sort.SliceStable(issues, func(i, j int) bool {
		if issues[i].Code != issues[j].Code {
			return issues[i].Code < issues[j].Code
		}
		if issues[i].Target != issues[j].Target {
			return issues[i].Target < issues[j].Target
		}
		return issues[i].Message < issues[j].Message
	})
}
