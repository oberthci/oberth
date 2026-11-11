package app

import (
	"os"
	"path/filepath"
	"testing"
)

func groupedPipeline(annotations string) string {
	return `apiVersion: argoproj.io/v1alpha1
kind: Workflow
metadata:
  annotations:
    example.com/tier: M
` + annotations + `spec:
  entrypoint: main
  activeDeadlineSeconds: 3600
  templates:
    - name: main
      container:
        image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
        command: ["echo", "ok"]
`
}

func writePipeline(t *testing.T, dir, file, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".oberth"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".oberth", file), []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The group is read from the document the run executes: build.yaml for branch
