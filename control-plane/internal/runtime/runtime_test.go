package runtime

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func spec(key string, deps ...string) TaskSpec {
	return TaskSpec{Key: key, AgentType: "requirements_analyst", Objective: "do " + key, DependsOn: deps}
}

func TestValidatePlanTopologicalOrder(t *testing.T) {
	plan := []TaskSpec{spec("d", "b", "c"), spec("b", "a"), spec("c", "a"), spec("a")}
	order, err := ValidatePlan(plan, 5)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(order, []string{"a", "b", "c", "d"}) {
		t.Fatalf("order: %v", order)
	}
	if plan[0].BudgetUSD != DefaultTaskBudgetUSD || plan[0].MaxAttempts != DefaultMaxAttempts {
		t.Fatalf("defaults not applied: %+v", plan[0])
	}
}

func TestValidatePlanRejections(t *testing.T) {
	cases := map[string]struct {
		plan []TaskSpec
		want string
	}{
		"empty":        {nil, "no tasks"},
		"cycle":        {[]TaskSpec{spec("a", "c"), spec("b", "a"), spec("c", "b"), spec("x")}, "cycle among tasks a, b, c"},
		"self edge":    {[]TaskSpec{spec("a", "a")}, "depends on itself"},
		"unknown dep":  {[]TaskSpec{spec("a", "zzz")}, "unknown task"},
		"duplicate":    {[]TaskSpec{spec("a"), spec("a")}, "duplicate"},
		"bad key":      {[]TaskSpec{spec("Bad Key")}, "must match"},
		"over budget":  {[]TaskSpec{{Key: "a", AgentType: "x", Objective: "o", BudgetUSD: 9}}, "within the run budget"},
		"no objective": {[]TaskSpec{{Key: "a", AgentType: "x"}}, "objective is required"},
		"bad cap":      {[]TaskSpec{{Key: "a", AgentType: "x", Objective: "o", GrantedCapabilities: []string{"Run Read"}}}, "invalid capability"},
	}
	for name, c := range cases {
		_, err := ValidatePlan(c.plan, 5)
		if err == nil || !errors.Is(err, ErrInvalidPlan) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want %q", name, err, c.want)
		}
	}
}
