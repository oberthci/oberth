package model

import "testing"

func TestWorkflowFileAlphabet(t *testing.T) {
	for _, file := range []string{".oberth/release.yaml", "workflows/Release_1-2.yml"} {
		if err := ValidateWorkflowFile(file); err != nil {
			t.Fatal(file, err)
		}
	}
	for _, file := range []string{"release file.yaml", "release\tfile.yaml", "release\x01.yaml", "release'file.yaml", "release\"file.yaml", "rél ease.yaml", "release:file.yaml", "release;file.yaml"} {
		if err := ValidateWorkflowFile(file); err == nil {
			t.Fatalf("accepted %q", file)
		}
	}
}
