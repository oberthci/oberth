package argoworkflow

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/pkg/periapsis"
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

func TestDeclaredSize(t *testing.T) {
	workflow, err := Decode([]byte(admissibleDocument))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	size, err := DeclaredSize(workflow)
	if err != nil || size != periapsis.M {
		t.Fatalf("undeclared size = %q,%v; want M", size, err)
	}
	for declared, expected := range map[string]periapsis.Size{
		"S": periapsis.S, "M": periapsis.M, "L": periapsis.L, "XL": periapsis.XL,
	} {
		workflow.Annotations = map[string]string{SizeAnnotation: declared}
		size, err := DeclaredSize(workflow)
		if err != nil || size != expected {
			t.Fatalf("size %q = %q,%v", declared, size, err)
		}
	}
	workflow.Annotations = map[string]string{SizeAnnotation: "XXL"}
	if _, err := DeclaredSize(workflow); err == nil {
		t.Fatal("expected an unknown size to be refused")
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

func TestAuthorizeSecretPathsAllowsBothTriggers(t *testing.T) {
	paths := []string{"oberth/data/release/cosign-secret"}
	allowlist := []string{"oberth/data/release/cosign-secret"}
	if err := AuthorizeSecretPaths(paths, allowlist, periapsis.TriggerRelease, "skipops", "oberth"); err != nil {
		t.Fatalf("allowlisted release path: %v", err)
	}
	// CI pipelines CANNOT declare system-namespace paths, even with an allowlist.
	if err := AuthorizeSecretPaths(paths, allowlist, periapsis.TriggerCI, "skipops", "oberth"); err == nil {
		t.Fatal("expected a system-namespace path to be refused for CI")
	}
	if err := AuthorizeSecretPaths(paths, nil, periapsis.TriggerRelease, "skipops", "oberth"); err == nil {
		t.Fatal("expected a non-allowlisted system path to be refused")
	}
	if err := AuthorizeSecretPaths(paths, nil, periapsis.TriggerCI, "skipops", "oberth"); err == nil {
		t.Fatal("expected a system path to be refused for CI")
	}
	if err := AuthorizeSecretPaths(nil, nil, periapsis.TriggerCI, "skipops", "oberth"); err != nil {
		t.Fatalf("declaring nothing is always fine: %v", err)
	}
	// Upstream-scoped paths are CI-eligible.
	upstreamPaths := []string{"oberth/upstream/skipops/oberth/test-secret"}
	if err := AuthorizeSecretPaths(upstreamPaths, nil, periapsis.TriggerCI, "skipops", "oberth"); err != nil {
		t.Fatalf("upstream-scoped CI path: %v", err)
	}
	if err := AuthorizeSecretPaths(upstreamPaths, nil, periapsis.TriggerRelease, "skipops", "oberth"); err != nil {
		t.Fatalf("upstream-scoped release path: %v", err)
	}
}

func TestAuthorizeSecretPathsScopesUpstreamNamespaceByRepository(t *testing.T) {
	own := []string{"oberth/upstream/skipops/oberth/gar-sa-key"}
	if err := AuthorizeSecretPaths(own, nil, periapsis.TriggerRelease, "skipops", "oberth"); err != nil {
		t.Fatalf("a repository's own upstream-scoped path (release): %v", err)
	}
	// Upstream-scoped paths also work for CI pipelines.
	if err := AuthorizeSecretPaths(own, nil, periapsis.TriggerCI, "skipops", "oberth"); err != nil {
		t.Fatalf("a repository's own upstream-scoped path (CI): %v", err)
	}
	stolen := []string{"oberth/upstream/skipops/acme-ebpf/gar-sa-key"}
	if err := AuthorizeSecretPaths(stolen, nil, periapsis.TriggerRelease, "skipops", "oberth"); err == nil {
		t.Fatal("expected another repository's scoped path to be refused")
	}
	foreignOrg := []string{"oberth/upstream/someone-else/oberth/gar-sa-key"}
	if err := AuthorizeSecretPaths(foreignOrg, nil, periapsis.TriggerRelease, "skipops", "oberth"); err == nil {
		t.Fatal("expected another org's path to be refused")
	}
}

// A leading `---` is legal YAML and idiomatic in plenty of repositories. The
// earlier guard re-searched the source from position 0 for every separator it
// found, so it always examined the FIRST one -- and with a leading separator
// that one has nothing before it. Every later separator therefore reported
// "no content before", the guard never fired, and UnmarshalStrict quietly
// decoded document one and discarded the rest. The control was resting on which
// document the YAML library happens to return rather than on the check.
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
