package workflowpolicy

import (
	"slices"
	"testing"
)

func TestSourcePolicyAllowsOnlyListedPairs(t *testing.T) {
	policy, err := ParseSourcePolicy([]byte(`{"origin/team/cli":["origin/team/cli","origin/team/lib","origin/team/lib"]}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		consumer, producer string
		want               bool
	}{
		{"origin/team/cli", "origin/team/cli", true},
		{"origin/team/cli", "origin/team/lib", true},
		{"origin/team/lib", "origin/team/lib", false},
		{"origin/team/lib", "origin/team/cli", false},
	} {
		if policy.Allows(tc.consumer, tc.producer) != tc.want {
			t.Errorf("%s <- %s: want %t", tc.consumer, tc.producer, tc.want)
		}
	}
	if got := policy.Pairs(); !slices.Equal(got, []string{"origin/team/cli=origin/team/cli", "origin/team/cli=origin/team/lib"}) {
		t.Fatal("pairs listed wrongly", got)
	}
	empty, err := ParseSourcePolicy(nil)
	if err != nil || empty.Allows("origin/team/cli", "origin/team/cli") || len(empty.Pairs()) != 0 {
		t.Fatal("empty policy allowed a pair", err)
	}
}

func TestSourcePolicyRefusesNoncanonicalNames(t *testing.T) {
	for _, raw := range []string{`[]`, `{"cli":[]}`, `{"origin/team/cli":["lib"]}`, `{"origin/team/cli":["-x/team/lib"]}`, `{"origin/team/cli":"origin/team/lib"}`} {
		if _, err := ParseSourcePolicy([]byte(raw)); err == nil {
			t.Errorf("%s accepted", raw)
		}
	}
}
