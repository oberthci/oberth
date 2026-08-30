package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fragmentConsumer = `apiVersion: argoproj.io/v1alpha1
kind: Workflow
metadata:
  annotations:
    oberth.ci/size: S
spec:
  entrypoint: main
  activeDeadlineSeconds: 600
  templates:
    - name: main
      steps:
        - - name: verify
            templateRef:
              name: acme/maven-verify@v3
              template: run
`

func writeConsumer(t *testing.T, body string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".oberth"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".oberth", "build.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestValidateRefusesToPassADocumentWithUnresolvedFragments(t *testing.T) {
	root := writeConsumer(t, fragmentConsumer)
	var output bytes.Buffer
	err := runValidate(context.Background(), []string{root}, &output)
	if err == nil {
		t.Fatal("validate passed a document whose fragments it never resolved")
	}
	if !strings.Contains(output.String(), "acme/maven-verify@v3") {
		t.Fatalf("validate did not name the unresolved reference:\n%s", output.String())
	}
	if strings.Contains(output.String(), "inline the template") {
		t.Fatalf("validate still tells the author to inline a legitimately pinned fragment:\n%s", output.String())
	}
}

func TestValidateReportsFragmentsAsUncheckedWhenAllowed(t *testing.T) {
	root := writeConsumer(t, fragmentConsumer)
	var output bytes.Buffer
	if err := runValidate(context.Background(), []string{"--allow-unresolved-fragments", root}, &output); err != nil {
		t.Fatalf("validate: %v\n%s", err, output.String())
	}
	if !strings.Contains(output.String(), "unchecked") {
		t.Fatalf("validate did not say the fragment contents are unchecked:\n%s", output.String())
	}
}

func TestValidateStillRefusesARawTemplateRefThatIsNotAFragment(t *testing.T) {
	body := strings.Replace(fragmentConsumer, "acme/maven-verify@v3", "some-cluster-template", 1)
	root := writeConsumer(t, body)
	var output bytes.Buffer
	if err := runValidate(context.Background(), []string{root}, &output); err == nil {
		t.Fatalf("validate accepted a templateRef that names no pinned fragment:\n%s", output.String())
	}
}
