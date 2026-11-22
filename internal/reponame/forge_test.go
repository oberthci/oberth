package reponame

import (
	"errors"
	"net/url"
	"testing"
)

func TestForgeURLAcceptsTheForgesOwnUserAndPath(t *testing.T) {
	for address, user := range map[string]string{
		"ssh://oberth@192.168.86.247/home/oberth/repos/oberth-smoke.git": "oberth",
		"ssh://git@forge.oberth.ci/team/widget.git":                      "git",
		"ssh://forge.oberth.ci:2222/srv/git/team/widget.git":             DefaultForgeUser,
		"ssh://oberth@192.168.86.247/~/repos/oberth-smoke.git":           "oberth",
		"https://forge.oberth.ci":                                        DefaultForgeUser,
	} {
		u, err := url.Parse(address)
		if err != nil {
			t.Fatal(err)
		}
		if err = ValidateForgeURL(u); err != nil || ForgeUser(u) != user {
			t.Fatal("forge address refused or user lost", address, ForgeUser(u), err)
		}
	}
}

// Every refused value would otherwise reach ssh or git as an option, a second
// configuration line or a path outside the registered namespace.
func TestForgeURLRefusesInjectedUsersAndPaths(t *testing.T) {
	for address, component := range map[string]string{
		"ssh://-oProxyCommand=x@forge.oberth.ci/team/widget.git":   "forge user",
		"ssh://a%0AProxyCommand%20x@forge.oberth.ci/team/w.git":    "forge user",
		"ssh://git:secret@forge.oberth.ci/team/widget.git":         "forge user",
		"ssh://@forge.oberth.ci/team/widget.git":                   "forge user",
		"ssh://a%20b@forge.oberth.ci/team/widget.git":              "forge user",
		"ssh://user..name@forge.oberth.ci/team/widget.git":         "forge user",
		"ssh://" + string(make([]byte, 33)) + "@forge.oberth.ci/x": "",
		"ssh://forge.oberth.ci/home/../other/widget.git":           "forge path",
		"ssh://forge.oberth.ci/home/-upload/widget.git":            "forge path",
		"ssh://forge.oberth.ci/home//widget.git":                   "forge path",
		"ssh://forge.oberth.ci/home/%2e%2e/widget.git":             "forge path",
		"ssh://forge.oberth.ci/home/a%0Ab/widget.git":              "forge path",
		"ssh://forge.oberth.ci/repos/~/widget.git":                 "forge path",
		"ssh://forge.oberth.ci/~user/repos/widget.git":             "forge path",
		"ssh://forge.oberth.ci/~/../widget.git":                    "forge path",
		"https://glpat-AbCdEf0123456789xyz@forge.oberth.ci/team":   "forge user",
		"https://oauth2@forge.oberth.ci/team":                      "forge user",
		"http://user@forge.oberth.ci/team":                         "forge user",
		"https://user:token@forge.oberth.ci/team":                  "forge user",
	} {
		u, err := url.Parse(address)
		if component == "" {
			if err == nil {
				t.Fatal("control characters parsed", address)
			}
			continue
		}
		if err != nil {
			t.Fatal(address, err)
		}
		var invalid *InvalidNameError
		if err = ValidateForgeURL(u); !errors.As(err, &invalid) || invalid.Component != component {
			t.Fatal("unsafe forge address accepted", address, err)
		}
	}
	long, _ := url.Parse("ssh://" + "a123456789012345678901234567890bc" + "@forge.oberth.ci/team/widget.git")
	if ValidateForgeURL(long) == nil {
		t.Fatal("33-character user accepted")
	}
}

func TestRepositoryURLKeepsTheUpstreamsUser(t *testing.T) {
	r := Repository{Upstream: "192-168-86-247", Organization: "repos", Name: "oberth-smoke"}
	const base = "ssh://oberth@192.168.86.247/home/oberth/repos"
	u, err := r.URL(base)
	if err != nil || u.String() != "ssh://oberth@192.168.86.247/home/oberth/repos/oberth-smoke.git" {
		t.Fatal(u, err)
	}
	for address, matches := range map[string]bool{
		"ssh://oberth@192.168.86.247/home/oberth/repos/oberth-smoke.git": true,
		"ssh://git@192.168.86.247/home/oberth/repos/oberth-smoke.git":    false,
		"ssh://192.168.86.247/home/oberth/repos/oberth-smoke.git":        false,
		"ssh://other@192.168.86.247/home/oberth/repos/oberth-smoke.git":  false,
	} {
		supplied, err := url.Parse(address)
		if err != nil {
			t.Fatal(err)
		}
		if r.MatchesURL(base, supplied) != matches {
			t.Fatal("user did not decide the match", address)
		}
	}
	plain, _ := url.Parse("ssh://git@forge.oberth.ci/team/widget.git")
	if !(Repository{Upstream: "origin", Organization: "team", Name: "widget"}).MatchesURL("ssh://forge.oberth.ci", plain) {
		t.Fatal("an address without a user does not log in as git")
	}
}
