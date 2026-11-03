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
              name: transferz/maven-verify@v3
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
	if !strings.Contains(output.String(), "transferz/maven-verify@v3") {
		t.Fatalf("validate did not name the unresolved reference:\n%s", output.String())
	}
	if strings.Contains(output.String(), "inline the template") {
		t.Fatalf("validate still tells the author to inline a legitimately pinned fragment:\n%s", output.String())
	}
}

func TestValidateRefusesUncheckedFragmentsEvenWhenAllowed(t *testing.T) {
	root := writeConsumer(t, fragmentConsumer)
	var output bytes.Buffer
	if err := runValidate(context.Background(), []string{"--allow-unresolved-fragments", root}, &output); err == nil {
		t.Fatalf("validate passed unchecked fragments:\n%s", output.String())
	}
	for _, want := range []string{"transferz/maven-verify@v3", "offline", "result: FAIL"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("validate output missing %q:\n%s", want, output.String())
		}
	}
	if strings.Contains(output.String(), "result: PASS") {
		t.Fatalf("validate reported success without admission:\n%s", output.String())
	}
}

// A fragment reference used to skip all admission, even for consumer-owned
// leaves. The same KVM cache mount must never pass just by adding a fragment.
func TestValidateFragmentCannotBypassKVMCacheMountAdmission(t *testing.T) {
	const document = `apiVersion: argoproj.io/v1alpha1
kind: Workflow
metadata:
  annotations:
    oberth.ci/size: S
    oberth.ci/kvm-templates: kernel-test
spec:
  entrypoint: main
  activeDeadlineSeconds: 600
  templates:
    - name: main
      steps:
        - - name: kernel-test
            template: kernel-test
    - name: kernel-test
      metadata:
        annotations:
          oberth.ci/workspace-mounts: cache
          oberth.ci/workspace-env: none
      automountServiceAccountToken: false
      container:
        image: golang:1.26.10-trixie@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
        command: [/bin/sh, -c]
        args: ["true"]
`
	for _, withFragment := range []bool{false, true} {
		name := "inline"
		body := document
		if withFragment {
			name = "fragment"
			body = strings.Replace(body, "            template: kernel-test\n", `            template: kernel-test
        - - name: verify
            templateRef:
              name: transferz/maven-verify@v3
              template: run
`, 1)
		}
		t.Run(name, func(t *testing.T) {
			root := writeConsumer(t, body)
			var output bytes.Buffer
			err := runValidate(context.Background(), []string{"--kubevirt", "--allow-unresolved-fragments", root}, &output)
			if err == nil {
				t.Fatalf("validate passed a KVM cache mount:\n%s", output.String())
			}
			want := `KVM template "kernel-test" cannot use oberth.ci/workspace-mounts: cache`
			if withFragment {
				// Without pinned contents available offline the entire document
				// must fail, rather than claiming the KVM check ran.
				want = "cannot be resolved offline"
			}
			if !strings.Contains(output.String(), want) || !strings.Contains(output.String(), "result: FAIL") {
				t.Fatalf("validate did not report %q:\n%s", want, output.String())
			}
		})
	}
}

func TestValidateStillRefusesARawTemplateRefThatIsNotAFragment(t *testing.T) {
	body := strings.Replace(fragmentConsumer, "transferz/maven-verify@v3", "some-cluster-template", 1)
	root := writeConsumer(t, body)
	var output bytes.Buffer
	if err := runValidate(context.Background(), []string{root}, &output); err == nil {
		t.Fatalf("validate accepted a templateRef that names no pinned fragment:\n%s", output.String())
	}
}
