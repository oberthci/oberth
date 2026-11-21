package reponame

import (
	"net/url"
	"testing"
)

func TestRepositoryURLUsesStoredAddressAndNamespace(t *testing.T) {
	r := Repository{Upstream: "origin", Organization: "team", Name: "widget"}
	for _, tc := range []struct {
		base, supplied string
		matches        bool
	}{
		{"ssh://forge.oberth.ci", "ssh://forge.oberth.ci/team/widget.git", true},
		{"ssh://forge.oberth.ci/team/", "ssh://forge.oberth.ci/team/widget", true},
		{"ssh://forge.oberth.ci/base/team", "ssh://forge.oberth.ci/base/team/widget.git", true},
		{"ssh://forge.oberth.ci/base/team", "ssh://forge.oberth.ci/team/widget.git", false},
		{"ssh://forge.oberth.ci", "ssh://other.oberth.ci/team/widget.git", false},
		{"ssh://forge.oberth.ci", "ssh://forge.oberth.ci:22/team/widget.git", false},
		{"ssh://forge.oberth.ci", "https://forge.oberth.ci/team/widget.git", false},
		{"ssh://forge.oberth.ci", "ssh://forge.oberth.ci/other/widget.git", false},
		{"ssh://forge.oberth.ci", "ssh://forge.oberth.ci/team/widget.git?ref=main", false},
	} {
		t.Run(tc.supplied+tc.base, func(t *testing.T) {
			u, err := url.Parse(tc.supplied)
			if err != nil {
				t.Fatal(err)
			}
			if got := r.MatchesURL(tc.base, u); got != tc.matches {
				t.Fatalf("match=%v, want %v", got, tc.matches)
			}
		})
	}
}
