package argojob

import (
	"fmt"
	"strings"
	"testing"
)

func TestReleaseTagRoutesResolveAndRefuse(t *testing.T) {
	routes := ReleaseTagRoutes{"github/org/repo": {{Pattern: "deploy-*", File: ".oberth/deploy.yaml"}}}
	if err := routes.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ repo, ref, want string }{
		{"github/org/repo", "refs/tags/deploy-1", ".oberth/deploy.yaml"},
		{"github/org/repo", "refs/tags/v1", ".oberth/release.yaml"},
		{"github/other/repo", "refs/tags/deploy-1", ".oberth/release.yaml"},
		{"codeberg/org/repo", "refs/tags/deploy-1", ".oberth/release.yaml"},
	} {
		got, err := routes.Resolve(tc.repo, tc.ref)
		if err != nil || got != tc.want {
			t.Fatalf("%+v: %q %v", tc, got, err)
		}
	}
	if _, err := routes.Resolve("github/org/repo", "refs/heads/deploy-1"); err == nil {
		t.Fatal("branch dispatch accepted")
	}
	snapshot := routes.Clone()
	routes["github/org/repo"][0].File = ".oberth/changed.yaml"
	if got, _ := snapshot.Resolve("github/org/repo", "refs/tags/deploy-1"); got != ".oberth/deploy.yaml" {
		t.Fatal("snapshot aliased caller")
	}
	snapshot["github/org/repo"] = append(snapshot["github/org/repo"], ReleaseTagRoute{Pattern: "*-1", File: ".oberth/other.yaml"})
	if _, err := snapshot.Resolve("github/org/repo", "refs/tags/deploy-1"); err == nil {
		t.Fatal("ambiguous patterns selected by order")
	}
}

func TestReleaseTagRoutesInvalidConfiguration(t *testing.T) {
	for _, file := range []string{"../../outside.yaml", "/tmp/file.yaml", ".oberth/../file.yaml", ".oberth/build.yaml", ".oberth/fragment.yaml", ".oberth/schedule.yaml", ".oberth/nested/release.yaml", ".oberth/Bad.yaml", ".oberth/file.go"} {
		if err := (ReleaseTagRoutes{"github/org/repo": {{Pattern: "deploy-*", File: file}}}).Validate(); err == nil {
			t.Errorf("unsafe file accepted: %q", file)
		}
	}
	for _, pattern := range []string{"", "[", "refs/tags/deploy-*", "bad\n*", strings.Repeat("*", 257)} {
		if err := (ReleaseTagRoutes{"github/org/repo": {{Pattern: pattern, File: ".oberth/deploy.yaml"}}}).Validate(); err == nil {
			t.Errorf("unsafe pattern accepted: %q", pattern)
		}
	}
	for _, repo := range []string{"org/repo", "/github/org/repo", "github/org/repo/", "github/org/repo/extra"} {
		if err := (ReleaseTagRoutes{repo: {{Pattern: "*", File: ".oberth/deploy.yaml"}}}).Validate(); err == nil {
			t.Errorf("noncanonical repo accepted: %q", repo)
		}
	}
	entries := []ReleaseTagRoute{}
	for i := range 33 {
		entries = append(entries, ReleaseTagRoute{Pattern: fmt.Sprintf("tag-%d-*", i), File: ".oberth/deploy.yaml"})
	}
	if err := (ReleaseTagRoutes{"github/org/repo": entries}).Validate(); err == nil {
		t.Fatal("unbounded routes")
	}
}
