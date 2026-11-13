package installer

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

const v021Defaults = "retention:\n  logs:\n    budgetBytes: 2147483648\n"

func defaultsDeps(user, all string, objects ...runtime.Object) Deps {
	return Deps{KubeClient: fake.NewClientset(objects...), RunHelm: func(_ context.Context, args []string) ([]byte, error) {
		if slices.Contains(args, "--all") {
			return []byte(all), nil
		}
		return []byte(user), nil
	}}
}

// An existing release keeps its rendered log budget (0) unless the operator
// chose one: the new 2 GiB default would delete old logs on the next sweep.
func TestUpgradeKeepsTheRenderedLogBudget(t *testing.T) {
	t.Parallel()
	all := `{"retention":{"logs":{"budgetBytes":0}}}`
	plan, err := reconcileChangedDefaults(t.Context(), UpgradeConfig{Namespace: "oberth"}, defaultsDeps(`{}`, all), "7", []byte(v021Defaults))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(plan.args, []string{"--set", "retention.logs.budgetBytes=0"}) || len(plan.notes) != 1 || !strings.Contains(plan.notes[0], "--set retention.logs.budgetBytes=2147483648") {
		t.Fatalf("plan = %+v", plan)
	}
	// Older stored values without a retention object render no budget: 0.
	plan, err = reconcileChangedDefaults(t.Context(), UpgradeConfig{Namespace: "oberth"}, defaultsDeps(`{}`, `{}`), "7", []byte(v021Defaults))
	if err != nil || !slices.Contains(plan.args, "retention.logs.budgetBytes=0") {
		t.Fatalf("older-chart plan = %+v, %v", plan, err)
	}
	// A stored or explicit choice is the operator's.
	for _, cfg := range []UpgradeConfig{{Namespace: "oberth", UserSet: []string{"retention.logs.budgetBytes=1024"}}, {Namespace: "oberth"}} {
		user := `{}`
		if len(cfg.UserSet) == 0 {
			user = `{"retention":{"logs":{"budgetBytes":0}}}`
		}
		plan, err := reconcileChangedDefaults(t.Context(), cfg, defaultsDeps(user, all), "7", []byte(v021Defaults))
		if err != nil || len(plan.args) != 0 {
			t.Fatalf("operator choice overridden: %+v, %v", plan, err)
		}
	}
	values := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(values, []byte("retention:\n  logs:\n    budgetBytes: 4096\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if plan, err := reconcileChangedDefaults(t.Context(), UpgradeConfig{Namespace: "oberth", UserValues: []string{values}}, defaultsDeps(`{}`, all), "7", []byte(v021Defaults)); err != nil || len(plan.args) != 0 {
		t.Fatalf("--values choice overridden: %+v, %v", plan, err)
	}
}

// The v0.20 installer's profile pin equals the profile the controller runs:
// a plain oberth upgrade clears it. A different, hand-set pin stays.
