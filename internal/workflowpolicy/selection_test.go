package workflowpolicy

import "testing"

func TestSelectSubmissionAndInspection(t *testing.T) {
	t.Parallel()
	for _, file := range []string{BuildFile, ReleaseFile, ".oberth/publish.yaml"} {
		routes := ReleaseRoutes{"forge/org/repo": {{Pattern: "v*", File: file}}}
		selected, release, err := Select(routes, SelectionRequest{
			Repository: "forge/org/repo", Ref: "refs/tags/v1.2.3", Tag: true, Release: true,
		})
		if err != nil || selected != file || !release {
			t.Fatal("release route lost its service decision", file, selected, release, err)
		}
		selected, release, err = Select(nil, SelectionRequest{File: "./" + file})
		if err != nil || selected != file || release != (file != BuildFile) {
			t.Fatal("file inspection lost its role", file, selected, release, err)
		}
		selected, release, err = Select(routes, SelectionRequest{
			Repository: "forge/org/repo", Ref: "refs/tags/v1.2.3", Tag: true,
		})
		if err != nil || selected != BuildFile || release {
			t.Fatal("ordinary tag gained release authority", selected, release, err)
		}
	}
}
