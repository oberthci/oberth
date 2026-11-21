package reponame

import (
	"errors"
	"strings"
	"testing"
)

func TestCanonicalRepositoryFields(t *testing.T) {
	for _, value := range []string{"origin/org/repo", "origin/a_1/repo.x-y", "a/0/" + strings.Repeat("x", 128)} {
		t.Run(value, func(t *testing.T) {
			got, err := ParseRepository(value)
			parts := strings.Split(value, "/")
			if err != nil || got != (Repository{Upstream: parts[0], Organization: parts[1], Name: parts[2]}) {
				t.Fatalf("parsed fields: %v %v", got, err)
			}
		})
	}
}

func TestRepositoryRefusals(t *testing.T) {
	for _, value := range []string{"", "a/b", "a/b/c/d", "/a/b/c", " a/b/c", "a/b/c.git", "a/b/c.GIT", "a/b/c.gIt", "a/b/..", "a/b/x..y", "a/.b/c", "a/b/c\n", "a/b/é", "a/b/" + strings.Repeat("x", 129), "release/b/c", "a/b/c\\d", "a/b/c%2fd"} {
		t.Run(value, func(t *testing.T) {
			_, err := ParseRepository(value)
			var target *InvalidNameError
			if !errors.As(err, &target) {
				t.Fatalf("missing typed refusal: %v", err)
			}
		})
	}
}

func TestUpstreamTrustNamespacesRefuse(t *testing.T) {
	for _, name := range []string{"release", "data", "upstream", "sys", "metadata", "metrics", "receive-outbox", "RELEASE", "Metadata", "mirror.git", "mirror.GIT", "", "../x"} {
		t.Run(name, func(t *testing.T) {
			var target *InvalidNameError
			if !errors.As(ValidateUpstreamName(name), &target) {
				t.Fatal("reserved/invalid upstream accepted")
			}
		})
	}
}

func TestRepoArgumentForms(t *testing.T) {
	for _, value := range []string{"widget", "Widget_1", "origin/team/widget"} {
		if err := ValidateSelector(value); err != nil {
			t.Fatalf("%q: %v", value, err)
		}
	}
	for _, value := range []string{"", "widget.GIT", "origin/team/widget.GIT", "team/widget", "/origin/team/widget", "../widget"} {
		if ValidateSelector(value) == nil {
			t.Fatalf("invalid selector %q accepted", value)
		}
	}
}
func TestUpstreamRegistrationSpelling(t *testing.T) {
	for _, name := range []string{"a", "0", "origin-1", strings.Repeat("a", 53)} {
		if err := ValidateUpstreamName(name); err != nil {
			t.Fatalf("valid upstream %q: %v", name, err)
		}
	}
	for _, name := range []string{"A", "a_b", "a.b", "-a", "a-", "a/b", "é", strings.Repeat("a", 54), "receive-outbox"} {
		if ValidateUpstreamName(name) == nil {
			t.Fatalf("invalid upstream %q accepted", name)
		}
	}
}

func TestRemediesMatchTheFailedComponent(t *testing.T) {
	cases := []struct{ value, component, example string }{
		{"widget", "repository path", "origin/team/widget"},
		{"origin/_team/widget", "organization", "team"},
		{"origin/team/widget.GIT", "repository name", "widget"},
	}
	for _, tc := range cases {
		_, err := ParseRepository(tc.value)
		var invalid *InvalidNameError
		if !errors.As(err, &invalid) || invalid.Component != tc.component {
			t.Fatalf("wrong refusal for %q: %v", tc.value, err)
		}
		if !strings.Contains(invalid.Remedy(), tc.example) || strings.Contains(invalid.Remedy(), "bare") {
			t.Fatalf("wrong advice for %q: %v", tc.value, err)
		}
	}
	if _, err := ParseRepository("origin/team/widget"); err != nil {
		t.Fatal("canonical remedy example is invalid", err)
	}
	if err := ValidateSelector("widget"); err != nil {
		t.Fatal("repository-name remedy example is invalid", err)
	}
}

func TestGlobalIsReservedAsAnUpstream(t *testing.T) {
	var invalid *InvalidNameError
	if !errors.As(ValidateUpstreamName("global"), &invalid) || !invalid.reserved {
		t.Fatal("global must retain its installation-scope meaning")
	}
	if _, err := ParseRepository("global/team/widget"); err == nil {
		t.Fatal("reserved upstream accepted in a repository")
	}
	if err := ValidateSelector("global"); err != nil {
		t.Fatal("a bare repository name is not an upstream", err)
	}
}
