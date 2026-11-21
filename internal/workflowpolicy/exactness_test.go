package workflowpolicy

import (
	"testing"

	"github.com/oberthci/oberth/internal/identityname"
)

func TestNamedAndWildcardGrantsPreserveExactTupleSpelling(t *testing.T) {
	for _, step := range []string{"publish", "*"} {
		t.Run(step, func(t *testing.T) {
			grant := GrantKey{Key: identityname.Key{Repository: "forge/org/repo", File: ".oberth/release.yaml", Step: step}, Path: "public/key"}
			key := identityname.Key{Repository: grant.Repository, File: grant.File, Step: "publish"}
			if !grant.Matches(key) {
				t.Fatal("exact tuple refused")
			}
			for _, candidate := range []identityname.Key{
				{Repository: "Forge/org/repo", File: key.File, Step: key.Step},
				{Repository: "forge/Org/repo", File: key.File, Step: key.Step},
				{Repository: "forge/org/Repo", File: key.File, Step: key.Step},
				{Repository: key.Repository, File: ".oberth/Release.yaml", Step: key.Step},
				{Repository: key.Repository, File: ".oberth/./release.yaml", Step: key.Step},
				{Repository: key.Repository, File: ".oberth//release.yaml", Step: key.Step},
				{Repository: "forge/org/repo/", File: key.File, Step: key.Step},
			} {
				if grant.Matches(candidate) {
					t.Fatalf("scope matched alternate spelling: %+v", candidate)
				}
			}
			if step != "*" {
				key.Step = "Publish"
				if grant.Matches(key) {
					t.Fatal("named step comparison ignored case")
				}
			}
		})
	}
}

func TestRoutePatternCharactersAreIndependentlyRefused(t *testing.T) {
	for _, tc := range []struct{ name, pattern string }{
		{"space", "v x"}, {"backslash", `v\x`}, {"nul", "v\x00x"},
		{"cr", "v\rx"}, {"lf", "v\nx"}, {"tab", "v\tx"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			routes := ReleaseRoutes{"forge/org/repo": {{Pattern: tc.pattern, File: ReleaseFile}}}
			requireInvalid(t, routes.Validate())
		})
	}
}
