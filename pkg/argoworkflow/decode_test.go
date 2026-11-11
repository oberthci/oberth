package argoworkflow

import (
	"strings"
	"testing"
)

func TestDecodeRejectsNonWorkflowKinds(t *testing.T) {
	for _, kind := range []string{"WorkflowTemplate", "ClusterWorkflowTemplate", "CronWorkflow", "Pod"} {
		document := strings.Replace(admissibleDocument, "kind: Workflow\n", "kind: "+kind+"\n", 1)
		if _, err := Decode([]byte(document)); err == nil {
			t.Fatalf("expected kind %q to be refused", kind)
		}
	}
}

func TestDecodeRejectsForeignAPIVersions(t *testing.T) {
	document := strings.Replace(admissibleDocument, "apiVersion: argoproj.io/v1alpha1", "apiVersion: batch/v1", 1)
	if _, err := Decode([]byte(document)); err == nil {
		t.Fatal("expected a foreign apiVersion to be refused")
	}
}

// TestDecodeIsStrict proves an unreviewed field cannot ride along silently. A
// lenient decoder would drop a misspelling; the gate reasons about named
// fields, so a dropped field is a permission nobody reviewed.
//
// Note the case-insensitivity caveat below: Go's JSON decoder matches field
// names case-insensitively, so "podSpecPatch" and "podspecpatch" are the same
// field, not an unknown one. That works in the gate's favour — a
// differently-cased spelling of a refused field is still refused.
func TestDecodeIsStrict(t *testing.T) {
	document := strings.Replace(admissibleDocument, "  entrypoint: main\n",
		"  entrypoint: main\n  entrypointt: typo\n", 1)
	if _, err := Decode([]byte(document)); err == nil {
		t.Fatal("expected an unknown field to be refused")
	}
}

func TestAdmitRefusesDifferentlyCasedPodSpecPatch(t *testing.T) {
	document := strings.Replace(admissibleDocument, "  entrypoint: main\n",
		"  entrypoint: main\n  podspecpatch: '{\"serviceAccountName\":\"oberth-argo-release\"}'\n", 1)
	refuse(t, document, Policy{}, "podSpecPatch")
}

func TestDecodeRejectsMultipleDocuments(t *testing.T) {
	document := admissibleDocument + "\n---\n" + admissibleDocument
	if _, err := Decode([]byte(document)); err == nil {
		t.Fatal("expected a multi-document stream to be refused")
	}
}

func TestDecodeAcceptsALeadingDocumentSeparator(t *testing.T) {
	if _, err := Decode([]byte("---\n" + strings.TrimPrefix(admissibleDocument, "\n"))); err != nil {
		t.Fatalf("a leading separator is one document: %v", err)
	}
}

func TestDecodeRejectsEmptyAndOversizedInput(t *testing.T) {
	if _, err := Decode(nil); err == nil {
		t.Fatal("expected empty input to be refused")
	}
	if _, err := Decode([]byte("   \n\t\n")); err == nil {
		t.Fatal("expected whitespace-only input to be refused")
	}
	oversized := make([]byte, MaxSourceBytes+1)
	for index := range oversized {
		oversized[index] = ' '
	}
	if _, err := Decode(oversized); err == nil {
		t.Fatal("expected oversized input to be refused")
	}
}

func TestDeclaredSecretPaths(t *testing.T) {
	workflow, err := Decode([]byte(admissibleDocument))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if paths, err := DeclaredSecretPaths(workflow); err != nil || paths != nil {
		t.Fatalf("undeclared paths = %v,%v", paths, err)
	}
	workflow.Annotations = map[string]string{
		SecretPathsAnnotation: "oberth/data/release/r2-upload-token, oberth/data/release/cosign-secret",
	}
	paths, err := DeclaredSecretPaths(workflow)
	if err != nil {
		t.Fatalf("declared paths: %v", err)
	}
	if len(paths) != 2 || paths[0] != "oberth/data/release/r2-upload-token" {
		t.Fatalf("paths = %v", paths)
	}

	workflow.Annotations[SecretPathsAnnotation] = "oberth/data/a, oberth/data/a"
	if _, err := DeclaredSecretPaths(workflow); err == nil {
		t.Fatal("expected a duplicated path to be refused")
	}
	workflow.Annotations[SecretPathsAnnotation] = "not a path"
	if _, err := DeclaredSecretPaths(workflow); err == nil {
		t.Fatal("expected a malformed path to be refused")
	}
}

func TestDecodeRejectsSecondDocumentHiddenBehindALeadingSeparator(t *testing.T) {
	t.Parallel()
	source := []byte(`---
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  activeDeadlineSeconds: 3600
  templates:
    - name: main
      container:
        image: golang:1.26-alpine
        command: [/bin/true]
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: smuggled
`)
	if _, err := Decode(source); err == nil {
		t.Fatal("a second document hidden behind a leading separator was accepted")
	}
}

// A leading separator on its own opens the first document and is not a second
// one. Refusing it would reject ordinary, correct files.
func TestDecodeAcceptsALeadingSeparator(t *testing.T) {
	t.Parallel()
	source := []byte(`---
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  activeDeadlineSeconds: 3600
  templates:
    - name: main
      container:
        image: golang:1.26-alpine
        command: [/bin/true]
`)
	if _, err := Decode(source); err != nil {
		t.Fatalf("a single document opened by a separator was refused: %v", err)
	}
}

func TestDecodeAcceptsIndentedSeparatorsInBlockScalars(t *testing.T) {
	t.Parallel()
	source := []byte(`apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  activeDeadlineSeconds: 3600
  templates:
    - name: main
      script:
        image: golang:1.26-alpine
        command: [/bin/sh]
        source: |
          echo "before"
          ---
          echo "after"
          ...
          echo "done"
`)
	if err := rejectMultipleDocuments(source); err != nil {
		t.Fatalf("indented --- and ... inside a block scalar were rejected: %v", err)
	}
}

func TestDecodeAcceptsTrailingDocumentEnd(t *testing.T) {
	t.Parallel()
	source := []byte(`apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  activeDeadlineSeconds: 3600
  templates:
    - name: main
      container:
        image: golang:1.26-alpine
        command: [/bin/true]
...
`)
	if err := rejectMultipleDocuments(source); err != nil {
		t.Fatalf("a trailing ... document-end marker was rejected: %v", err)
	}
}

func TestDecodeRejectsContentAfterDocumentEnd(t *testing.T) {
	t.Parallel()
	source := []byte(`apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  activeDeadlineSeconds: 3600
  templates:
    - name: main
      container:
        image: golang:1.26-alpine
        command: [/bin/true]
...
apiVersion: v1
kind: ConfigMap
`)
	if err := rejectMultipleDocuments(source); err == nil {
		t.Fatal("content after a ... document-end marker was accepted")
	}
}
