package workflowpolicy

import "testing"

func TestManualSelectionCannotReachReleaseRoutes(t *testing.T) {
	routes := ReleaseRoutes{"forge/org/repo": {{Pattern: "v*", File: "workflows/publish.yaml"}}}
	for _, file := range []string{ReleaseFile, "workflows/publish.yaml", BuildFile, ".oberth/walk.yaml", "hack/walk-vm.yaml"} {
		q := SelectionRequest{Repository: "forge/org/repo", Ref: "refs/heads/topic", File: file, Manual: true}
		selected, release, err := Select(routes, q)
		blocked := file == ReleaseFile || file == "workflows/publish.yaml"
		if (err != nil) != blocked || release || !blocked && selected != file {
			t.Fatal(file, selected, release, err)
		}
		q.Ref, q.Tag = "refs/tags/test", true
		if _, _, err := Select(routes, q); err == nil {
			t.Fatal("tag override accepted")
		}
	}
	_, release, err := Select(routes, SelectionRequest{Repository: "forge/org/repo", File: "workflows/publish.yaml"})
	if err != nil || !release {
		t.Fatal("configured release inspection lost its tier", err)
	}
}
