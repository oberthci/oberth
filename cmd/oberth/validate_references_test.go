package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

func TestValidateRefusesExternalTemplateReferences(t *testing.T) {
	for _, reference := range []string{"shared/pipeline@v1", "shared-template"} {
		for _, call := range []struct {
			name, template string
		}{
			{"step", "    steps:\n    - - name: call\n        templateRef: {name: REFERENCE, template: run}\n"},
			{"dag", "    dag:\n      tasks:\n      - name: call\n        templateRef: {name: REFERENCE, template: run}\n"},
			{"hook", "    steps:\n    - - name: call\n        template: main\n        hooks:\n          exit:\n            templateRef: {name: REFERENCE, template: run}\n"},
			{"inline", "    steps:\n    - - name: outer\n        inline:\n          steps:\n          - - name: call\n              templateRef: {name: REFERENCE, template: run}\n"},
		} {
			t.Run(reference+"/"+call.name, func(t *testing.T) {
				body := "apiVersion: argoproj.io/v1alpha1\nkind: Workflow\nspec:\n  entrypoint: main\n  templates:\n  - name: main\n" + strings.ReplaceAll(call.template, "REFERENCE", reference)
				var output bytes.Buffer
				err := runValidate(t.Context(), []string{writeConsumer(t, body)}, &output)
				if err == nil || !strings.Contains(output.String(), "templateRef points outside this document") || !strings.Contains(output.String(), "result: FAIL") {
					t.Fatalf("external reference admission = %v:\n%s", err, output.String())
				}
			})
		}
	}
	t.Run("workflowTemplateRef", func(t *testing.T) {
		var output bytes.Buffer
		body := "apiVersion: argoproj.io/v1alpha1\nkind: Workflow\nspec:\n  workflowTemplateRef: {name: shared-template}\n"
		err := runValidate(t.Context(), []string{writeConsumer(t, body)}, &output)
		if err == nil || !strings.Contains(output.String(), "spec.workflowTemplateRef") || !strings.Contains(output.String(), "result: FAIL") {
			t.Fatalf("whole-spec reference admission = %v:\n%s", err, output.String())
		}
	})
}

func TestValidateReferencesDoNotBypassHostAccessAdmission(t *testing.T) {
	body := "apiVersion: argoproj.io/v1alpha1\nkind: Workflow\nspec:\n  hostNetwork: true\n  entrypoint: main\n  templates:\n  - name: main\n    steps:\n    - - name: call\n        templateRef: {name: shared/pipeline@v1, template: run}\n"
	var output bytes.Buffer
	err := runValidate(t.Context(), []string{writeConsumer(t, body)}, &output)
	for _, want := range []string{"hostNetwork", "templateRef points outside this document", "result: FAIL"} {
		if err == nil || !strings.Contains(output.String(), want) {
			t.Fatalf("admission omitted %q: %v:\n%s", want, err, output.String())
		}
	}
}

func TestRemovedFragmentCLIIsUnavailable(t *testing.T) {
	var output bytes.Buffer
	if err := runCLI(context.Background(), []string{"fragments", "list"}, nil, &output); !errors.Is(err, errUsage) {
		t.Fatalf("removed command = %v", err)
	}
	if err := runValidate(context.Background(), []string{"--allow-unresolved-fragments"}, &output); !errors.Is(err, errUsage) {
		t.Fatalf("removed validation flag = %v", err)
	}
	if _, err := parseServeOptions([]string{"--fragment-allowlist", "shared/pipeline"}, &output); !errors.Is(err, errUsage) {
		t.Fatalf("removed server flag = %v", err)
	}
}
