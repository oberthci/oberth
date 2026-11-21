package workflowpolicy

import "testing"

func TestSelectedWorkflowFileKind(t *testing.T) {
	t.Parallel()
	for _, scenario := range []struct {
		file    string
		release bool
	}{
		{"", false},
		{BuildFile, false},
		{ReleaseFile, true},
		{".oberth/publish.yaml", true},
		{"workflows/release.yaml", true},
	} {
		_, got, err := Select(nil, SelectionRequest{Repository: "forge/org/repo", File: scenario.file})
		if err != nil || got != scenario.release {
			t.Errorf("file %q: release = %v, want %v", scenario.file, got, scenario.release)
		}
	}
}
