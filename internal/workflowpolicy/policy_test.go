package workflowpolicy

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
)

func requireInvalid(t *testing.T, err error) {
	t.Helper()
	var invalid *InvalidError
	if !errors.As(errors.Join(errors.New("another"), err), &invalid) || invalid.Path == "" {
		t.Fatalf("missing typed refusal: %v", err)
	}
	for text, limit := range map[string]int{invalid.Reason(): 80, invalid.Remedy(): 120} {
		if problem := publicerr.CheckMessage(text, limit); problem != nil {
			t.Fatal(problem, text)
		}
	}
}

func TestFilesAreExactAndRelative(t *testing.T) {
	for _, file := range []string{BuildFile, ReleaseFile, "workflows/Release_1-2.yml", strings.Repeat("a", MaxFileBytes)} {
		if err := ValidateFile(file); err != nil {
			t.Fatal(file, err)
		}
	}
	for _, file := range []string{"", ".", "..", "../x", "/x", "x//y", "x/./y", "x/../y", "x/", "x\\y", "x*.yaml", "x?.yaml", "x[1]", "x\x00y", "x\ny", "x y", "é.yaml", strings.Repeat("a", MaxFileBytes+1)} {
		requireInvalid(t, ValidateFile(file))
	}
}

func TestReleaseRoutesDefaultExactMatchAndFileInventory(t *testing.T) {
	var empty ReleaseRoutes
	if err := empty.Validate(); err != nil {
		t.Fatal(err)
	}
	routes := ReleaseRoutes{"forge/org/repo": {{Pattern: "v*", File: ".oberth/version.yaml"}, {Pattern: "release-*", File: ReleaseFile}, {Pattern: "patch-*", File: ".oberth/version.yaml"}}}
	for _, tc := range []struct{ tag, file, pattern string }{{"v1.0.0", ".oberth/version.yaml", "v*"}, {"ordinary", ReleaseFile, ""}, {"release-one", ReleaseFile, "release-*"}} {
		got, err := routes.Resolve("forge/org/repo", tc.tag)
		if err != nil || got != (ReleaseRoute{Pattern: tc.pattern, File: tc.file}) {
			t.Fatal(got, err)
		}
	}
	got, err := empty.Resolve("forge/org/repo", "v1")
	if err != nil || got.File != ReleaseFile {
		t.Fatal(got, err)
	}
	if !reflect.DeepEqual(routes.Files("forge/org/repo"), []string{ReleaseFile, ".oberth/version.yaml"}) {
		t.Fatal("wrong file inventory")
	}
	for _, tag := range []string{"", "refs/tags/v1"} {
		_, err := routes.Resolve("forge/org/repo", tag)
		requireInvalid(t, err)
	}
	_, err = empty.Resolve("bare", "v1")
	requireInvalid(t, err)
}

func TestReleaseRoutesRefuseAmbiguityAndInvalidPolicy(t *testing.T) {
	routes := ReleaseRoutes{"forge/org/repo": {{Pattern: "v*", File: ReleaseFile}, {Pattern: "*1", File: ".oberth/other.yaml"}}}
	_, err := routes.Resolve("forge/org/repo", "v1")
	requireInvalid(t, err)
	for _, file := range []string{"workflows/Release_1.yml", ".oberth/nested/release.yaml", ".oberth/UPPER.yaml"} {
		if err := (ReleaseRoutes{"forge/org/repo": {{Pattern: "v*", File: file}}}).Validate(); err != nil {
			t.Fatal(file, err)
		}
	}
	for _, route := range []ReleaseRoute{{Pattern: "", File: ReleaseFile}, {Pattern: "[", File: ReleaseFile}, {Pattern: "refs/tags/*", File: ReleaseFile},
		{Pattern: "v *", File: ReleaseFile}, {Pattern: strings.Repeat("v", MaxRoutePatternBytes+1), File: ReleaseFile}, {Pattern: "v*", File: "../release.yaml"}, {Pattern: "v*", File: "*.yaml"}} {
		requireInvalid(t, (ReleaseRoutes{"forge/org/repo": {route}}).Validate())
	}
	requireInvalid(t, (ReleaseRoutes{"bare": {{Pattern: "v*", File: ReleaseFile}}}).Validate())
	requireInvalid(t, (ReleaseRoutes{"forge/org/repo": {}}).Validate())
	requireInvalid(t, (ReleaseRoutes{"forge/org/repo": {{Pattern: "v*", File: ReleaseFile}, {Pattern: "v*", File: ReleaseFile}}}).Validate())
}

func TestReleaseRoutesCloneIsIndependent(t *testing.T) {
	routes := ReleaseRoutes{"forge/org/repo": {{Pattern: "v*", File: ReleaseFile}}}
	copy := routes.Clone()
	copy["forge/org/repo"][0].File = "changed"
	copy["new"] = nil
	if routes["forge/org/repo"][0].File != ReleaseFile || len(routes) != 1 {
		t.Fatal("route clone aliases original")
	}
	if (ReleaseRoutes(nil)).Clone() != nil {
		t.Fatal("nil routes changed")
	}
}

func TestRouteCountLimitsRefuseOtherwiseValidPolicy(t *testing.T) {
	repos := make(ReleaseRoutes)
	for i := range MaxRouteRepositories {
		repos[fmt.Sprintf("forge/org/r%d", i)] = []ReleaseRoute{{Pattern: "v*", File: ReleaseFile}}
	}
	if err := repos.Validate(); err != nil {
		t.Fatal(err)
	}
	repos["forge/org/overflow"] = []ReleaseRoute{{Pattern: "v*", File: ReleaseFile}}
	requireInvalid(t, repos.Validate())
	routes := make([]ReleaseRoute, MaxRoutesPerRepository+1)
	for i := range routes {
		routes[i] = ReleaseRoute{Pattern: fmt.Sprintf("v%d", i), File: ReleaseFile}
	}
	if err := (ReleaseRoutes{"forge/org/repo": routes[:MaxRoutesPerRepository]}).Validate(); err != nil {
		t.Fatal(err)
	}
	requireInvalid(t, (ReleaseRoutes{"forge/org/repo": routes}).Validate())
}

func TestRouteRefusalsNameTheirRepositoryAndPattern(t *testing.T) {
	for _, routes := range []ReleaseRoutes{
		{"forge/org/repo": {}},
		{"forge/org/repo": {{Pattern: "[", File: ReleaseFile}}},
		{"forge/org/repo": {{Pattern: "v*", File: ReleaseFile}, {Pattern: "v*", File: ReleaseFile}}},
	} {
		err := routes.Validate()
		requireInvalid(t, err)
		if !strings.Contains(err.Error(), "forge/org/repo") {
			t.Fatal("missing repository context", err)
		}
	}
	routes := ReleaseRoutes{"forge/org/repo": {{Pattern: "[", File: ReleaseFile}}}
	err := routes.Validate()
	if !strings.Contains(err.Error(), `pattern "["`) {
		t.Fatal("missing pattern context", err)
	}
	_, err = routes.Resolve("forge/org/repo", "v1")
	requireInvalid(t, err)
	fileRoute := ReleaseRoutes{"forge/org/repo": {{Pattern: "v*", File: "../release.yaml"}}}
	fileError := fileRoute.Validate()
	var fileDetail *InvalidError
	if !errors.As(fileError, &fileDetail) || strings.Count(fileError.Error(), fileDetail.Remedy()) != 1 {
		t.Fatalf("file refusal must give its remedy once: %v", fileError)
	}

	invalid := ReleaseRoutes{"bare": {{Pattern: "v*", File: ReleaseFile}}}
	err = invalid.Validate()
	var detail *InvalidError
	if !errors.As(err, &detail) || detail.Path != "repo" {
		t.Fatal("repository remedy cannot be selected", err)
	}
}

func TestLongRouteContextDoesNotExtendReasons(t *testing.T) {
	tag := strings.Repeat("v", 1000)
	routes := ReleaseRoutes{"forge/org/repo": {{Pattern: "*", File: ReleaseFile}, {Pattern: "v*", File: ReleaseFile}}}
	_, err := routes.Resolve("forge/org/repo", tag)
	requireInvalid(t, err)
	if !strings.Contains(err.Error(), tag) {
		t.Fatal("tag context lost", err)
	}
	repo := strings.Repeat("repo", 1000)
	err = (ReleaseRoutes{repo: {{Pattern: "v*", File: ReleaseFile}}}).Validate()
	requireInvalid(t, err)
	if !strings.Contains(err.Error(), repo) {
		t.Fatal("repository context lost", err)
	}
}
