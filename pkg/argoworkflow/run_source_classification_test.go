package argoworkflow

import (
	"errors"
	"strings"
	"testing"
)

func TestResolvedFragmentDocumentRulesAndIntegrityHaveDistinctClasses(t *testing.T) {
	const consumer = "apiVersion: argoproj.io/v1alpha1\nkind: Workflow\nspec:\n  entrypoint: main\n  templates:\n  - name: main\n    steps:\n    - - name: call\n        templateRef: {name: forge/org/repo@v1, template: run}\n"
	const fragment = "apiVersion: argoproj.io/v1alpha1\nkind: Workflow\nspec:\n  entrypoint: run\n  templates:\n  - name: run\n    container:\n      image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n      command: [/bin/true]\n"
	key := FragmentKey{Repo: "forge/org/repo", Version: "v1"}
	for _, kind := range []string{"bad-reference", "missing-template", "nested-fragment", "bad-fragment-yaml", "whole-spec-reference", "missing-loaded-content", "wrong-digest"} {
		t.Run(kind, func(t *testing.T) {
			source := consumer
			fragments := map[FragmentKey]Fragment{key: {Key: key, SHA: strings.Repeat("a", 40), Source: []byte(fragment)}}
			document := true
			switch kind {
			case "bad-reference":
				source = strings.Replace(source, "forge/org/repo@v1", "unpinned", 1)
			case "missing-template":
				source = strings.Replace(source, "template: run", "template: absent", 1)
			case "nested-fragment":
				f := fragments[key]
				f.Source = []byte(consumer)
				fragments[key] = f
			case "bad-fragment-yaml":
				f := fragments[key]
				f.Source = []byte("kind: Secret")
				fragments[key] = f
			case "whole-spec-reference":
				source = strings.Replace(source, "  entrypoint:", "  workflowTemplateRef: {name: other}\n  entrypoint:", 1)
			case "missing-loaded-content":
				fragments = nil
				document = false
			case "wrong-digest":
				f := fragments[key]
				f.Digest = strings.Repeat("b", 64)
				fragments[key] = f
				document = false
			}
			_, err := ResolveRunSource([]byte(source), fragments, RunSourceBinding{})
			if err == nil {
				t.Fatal("fixture did not fail")
			}
			var refusal *AdmissionError
			if errors.As(err, &refusal) != document {
				t.Fatal("incorrect failure class", kind, err)
			}
		})
	}
}

func TestFragmentReferenceDiscoveryTypesAuthoredSyntaxErrors(t *testing.T) {
	workflow, err := Decode([]byte("apiVersion: argoproj.io/v1alpha1\nkind: Workflow\nspec:\n  templates:\n  - name: main\n    steps:\n    - - name: call\n        templateRef: {name: unpinned, template: run}\n"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = FragmentRefs(workflow)
	var refusal *AdmissionError
	if !errors.As(err, &refusal) {
		t.Fatal("pre-submission fragment discovery hid the document error", err)
	}
}
