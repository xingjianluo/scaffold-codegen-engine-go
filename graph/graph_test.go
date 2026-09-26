package graph

import (
	"testing"

	"github.com/xingjianluo/scaffold-codegen-engine-go/plan"
)

func TestDeterministicOrder(t *testing.T) {
	p := &plan.Plan{Name: "p", Version: 1, Targets: []plan.Target{
		{Name: "zeta", OutputPath: "z", Template: "z"},
		{Name: "alpha", OutputPath: "a", Template: "a"},
		{Name: "mid", OutputPath: "m", Template: "m", DependsOn: []string{"alpha"}},
		{Name: "beta", OutputPath: "b", Template: "b", DependsOn: []string{"alpha"}},
	}}
	g, err := Build(p)
	if err != nil {
		t.Fatal(err)
	}
	// Run several times; order must be identical regardless of map iteration.
	var want []string
	for i := 0; i < 50; i++ {
		got := g.Order()
		if i == 0 {
			want = got
			continue
		}
		if !eq(got, want) {
			t.Fatalf("nondeterministic order: %v vs %v", got, want)
		}
	}
	t.Logf("processing order: %v", want)
	if want[0] != "alpha" {
		t.Fatalf("alpha must be first, got %v", want)
	}
	// Dependents of alpha are beta and mid.
	deps := g.Dependents("alpha")
	if !eq(deps, []string{"beta", "mid"}) {
		t.Fatalf("dependents = %v", deps)
	}
}

func TestCycleError(t *testing.T) {
	p := &plan.Plan{Name: "p", Version: 1, Targets: []plan.Target{
		{Name: "a", OutputPath: "a", Template: "a", DependsOn: []string{"b"}},
		{Name: "b", OutputPath: "b", Template: "b", DependsOn: []string{"a"}},
	}}
	if _, err := Build(p); err == nil {
		t.Fatal("expected cycle error")
	}
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
