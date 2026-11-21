package reponame

import (
	"net/url"
	"testing"
)

func TestRepositoryURLRejectsAnEmptyQueryDelimiter(t *testing.T) {
	repository := Repository{Upstream: "origin", Organization: "team", Name: "widget"}
	const base = "ssh://forge.oberth.ci"
	for _, suffix := range []string{"", "?", "?ref=main"} {
		address, err := url.Parse(base + "/team/widget.git" + suffix)
		if err != nil {
			t.Fatal(err)
		}
		if got := repository.MatchesURL(base, address); got != (suffix == "") {
			t.Fatal("repository address query was ignored", suffix, got)
		}
	}
}
