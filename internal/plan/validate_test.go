package plan

import (
	"strings"
	"testing"
)

func hasIssue(issues Issues, code IssueCode, target string) bool {
	for _, is := range issues {
		if is.Code == code && is.Target == target {
			return true
		}
	}
	return false
}

func TestValidate_Cycle(t *testing.T) {
	p := &Plan{
		Targets: map[string]*Target{
			"a": {Path: "a.txt", Template: "a", Deps: []string{"b"}},
			"b": {Path: "b.txt", Template: "b", Deps: []string{"c"}},
			"c": {Path: "c.txt", Template: "c", Deps: []string{"a"}},
		},
	}
	issues := Validate(p)
	if len(issues) == 0 {
		t.Fatal("expected cycle issues, got none")
	}
	for _, is := range issues {
		if is.Code != IssueCycle {
			t.Fatalf("expected only cycle issues, got %+v", is)
		}
	}
	if !strings.Contains(issues[0].Message, "a -> b -> c -> a") {
		t.Fatalf("cycle message not explainable: %q", issues[0].Message)
	}
	if _, execIssues := ExecutionOrder(p); len(execIssues) == 0 {
		t.Fatal("ExecutionOrder must reject a cyclic plan")
	}
	// Deterministic: validate twice, compare.
	second := Validate(p)
	if second.Error() != issues.Error() {
		t.Fatalf("non-deterministic validation:\n%s\nvs\n%s", issues.Error(), second.Error())
	}
}

func TestValidate_SelfDependency(t *testing.T) {
	p := &Plan{Targets: map[string]*Target{
		"a": {Path: "a.txt", Deps: []string{"a"}},
	}}
	if !hasIssue(Validate(p), IssueSelfDep, "a") {
		t.Fatal("expected self_dep issue")
	}
}

func TestValidate_MissingDep(t *testing.T) {
	p := &Plan{Targets: map[string]*Target{
		"a": {Path: "a.txt", Deps: []string{"ghost"}},
	}}
	if !hasIssue(Validate(p), IssueMissingDep, "a") {
		t.Fatal("expected missing_dep issue")
	}
}

func TestValidate_MissingParam(t *testing.T) {
	p := &Plan{
		Params:  map[string]string{"name": ""},
		Targets: map[string]*Target{"a": {Path: "a.txt", RequiredParams: []string{"name", "who"}}},
	}
	issues := Validate(p)
	if !hasIssue(issues, IssueMissingParam, "a") {
		t.Fatal("expected missing_param issue")
	}
	if len(issues) != 2 {
		t.Fatalf("expected 2 missing-param issues (empty + absent), got %d: %v", len(issues), issues)
	}
}

func TestValidate_PathRules(t *testing.T) {
	cases := []struct {
		path string
		code IssueCode
	}{
		{"", IssueEmptyPath},
		{"/etc/passwd", IssuePathAbsolute},
		{"../x.txt", IssuePathTraversal},
		{"dir/../x.txt", IssuePathNotNormalized},
		{"./x.txt", IssuePathNotNormalized},
		{"dir//x.txt", IssuePathNotNormalized},
	}
	for _, tc := range cases {
		p := &Plan{Targets: map[string]*Target{"a": {Path: tc.path}}}
		if !hasIssue(Validate(p), tc.code, "a") {
			t.Fatalf("path %q: expected issue %s", tc.path, tc.code)
		}
	}
}

func TestValidate_PathConflict(t *testing.T) {
	p := &Plan{Targets: map[string]*Target{
		"a": {Path: "same.txt", Template: "1"},
		"b": {Path: "same.txt", Template: "2"},
	}}
	issues := Validate(p)
	if !hasIssue(issues, IssuePathConflict, "a") || !hasIssue(issues, IssuePathConflict, "b") {
		t.Fatalf("expected path_conflict on both owners, got %v", issues)
	}
}

func TestExecutionOrder_Deterministic(t *testing.T) {
	p := &Plan{Targets: map[string]*Target{
		"root":   {Path: "root.txt"},
		"mid":    {Path: "mid.txt", Deps: []string{"root"}},
		"leaf-a": {Path: "la.txt", Deps: []string{"mid"}},
		"leaf-b": {Path: "lb.txt", Deps: []string{"mid"}},
		"orphan": {Path: "o.txt"},
	}}
	var first []string
	for i := 0; i < 20; i++ {
		order, issues := ExecutionOrder(p)
		if len(issues) != 0 {
			t.Fatalf("unexpected issues: %v", issues)
		}
		ids := make([]string, len(order))
		for i, tg := range order {
			ids[i] = tg.ID
		}
		pos := map[string]int{}
		for i, id := range ids {
			pos[id] = i
		}
		if pos["root"] >= pos["mid"] || pos["mid"] >= pos["leaf-a"] || pos["mid"] >= pos["leaf-b"] {
			t.Fatalf("dependency order violated: %v", ids)
		}
		if i == 0 {
			first = ids
		} else if strings.Join(ids, ",") != strings.Join(first, ",") {
			t.Fatalf("non-deterministic order: %v vs %v", first, ids)
		}
	}
}
