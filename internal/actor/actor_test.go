package actor

import "testing"

func TestRepositoryAuthority(t *testing.T) {
	cases := []struct {
		name    string
		actor   Actor
		repo    string
		allowed bool
	}{
		{"zero", Actor{}, "origin/org/repo", false},
		{"missing identity", Actor{AuthMethod: "cert"}, "origin/org/repo", false},
		{"missing authentication", Actor{Identity: "reader"}, "origin/org/repo", false},
		{"unscoped", Actor{Identity: "reader", AuthMethod: "cert"}, "origin/org/repo", true},
		{"empty nonnil scope", Actor{Identity: "reader", AuthMethod: "cert", Scope: []string{}}, "origin/org/repo", true},
		{"scoped", Actor{Identity: "reader", AuthMethod: "cert", Scope: []string{"origin"}}, "origin/org/repo", true},
		{"outside scope", Actor{Identity: "reader", AuthMethod: "cert", Scope: []string{"other"}}, "origin/org/repo", false},
		{"admin scope", Actor{Identity: "reader", AuthMethod: "cert", Admin: true, Scope: []string{"other"}}, "origin/org/repo", false},
		{"metrics only", Actor{Identity: "reader", AuthMethod: "cert", Scope: []string{"metrics"}}, "origin/org/repo", false},
		{"unscoped metrics upstream", Actor{Identity: "reader", AuthMethod: "cert"}, "metrics/org/repo", true},
		{"no repository", Actor{Identity: "reader", AuthMethod: "cert"}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.actor.CanTouchRepository(tc.repo); got != tc.allowed {
				t.Fatalf("allowed=%v want %v", got, tc.allowed)
			}
		})
	}
}
func TestCloneOwnsScopeAndPreservesAbsence(t *testing.T) {
	for _, scope := range [][]string{nil, {}, {"origin"}} {
		original := Actor{AuthMethod: "cert", Identity: "reader", Scope: scope}
		clone := original.Clone()
		if (clone.Scope == nil) != (scope == nil) {
			t.Fatal("scope absence changed")
		}
		if len(scope) > 0 {
			clone.Scope[0] = "other"
			if !original.CanTouchRepository("origin/org/repo") {
				t.Fatal("scope aliases")
			}
		}
	}
}
