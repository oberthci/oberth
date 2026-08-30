package argojob

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"

	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

const fragmentImage = "golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func fragmentDocument(templates string) []byte {
	return []byte(`
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: run
  templates:
` + templates)
}

func consumingDocument(annotations, ref string) string {
	return `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
metadata:
  annotations:
` + annotations + `spec:
  entrypoint: main
  activeDeadlineSeconds: 600
  templates:
    - name: main
      steps:
        - - name: verify
            templateRef:
              name: ` + ref + `
              template: run
`
}

func fragmentRequest(t *testing.T, source string, fragments map[argoworkflow.FragmentKey]argoworkflow.Fragment) Request {
	t.Helper()
	request := testRequest(periapsis.TriggerCI, source)
	request.Fragments = fragments
	return request
}

func plainFragment(t *testing.T, repo, version string) (argoworkflow.FragmentKey, argoworkflow.Fragment) {
	t.Helper()
	key := argoworkflow.FragmentKey{Repo: repo, Version: version}
	return key, argoworkflow.Fragment{
		Key: key,
		SHA: "1111111111111111111111111111111111111111",
		Source: fragmentDocument(`    - name: run
      container:
        image: ` + fragmentImage + `
        command: [/bin/true]
`),
	}
}

func remainingRefs(workflow *wfv1.Workflow) int {
	count := 0
	for _, template := range workflow.Spec.Templates {
		for _, group := range template.Steps {
			for _, step := range group.Steps {
				if step.TemplateRef != nil {
					count++
				}
			}
		}
		if template.DAG != nil {
			for _, task := range template.DAG.Tasks {
				if task.TemplateRef != nil {
					count++
				}
			}
		}
	}
	return count
}

func TestBuildResolvesAFragmentBeforeAdmission(t *testing.T) {
	key, fragment := plainFragment(t, "acme/maven-verify", "v3")
	request := fragmentRequest(t, consumingDocument("    oberth.ci/size: S\n", key.String()),
		map[argoworkflow.FragmentKey]argoworkflow.Fragment{key: fragment})

	workflow, err := Build(testConfig(), request)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if remainingRefs(workflow) != 0 {
		t.Fatal("a templateRef reached the submitted Workflow; the gate would have refused it")
	}
	if len(workflow.Spec.Templates) != 2 {
		t.Fatalf("expected the consuming template plus one inlined, got %d", len(workflow.Spec.Templates))
	}
	called := workflow.Spec.Templates[0].Steps[0].Steps[0].Template
	if called == "" {
		t.Fatal("the step names no template after resolution")
	}
	found := false
	for _, template := range workflow.Spec.Templates {
		if template.Name == called {
			found = true
		}
	}
	if !found {
		t.Fatalf("the step names %q, which the submitted Workflow does not define", called)
	}
}

func TestBuildChecksAFragmentsSecretPathsAgainstTheApprovalTable(t *testing.T) {
	key := argoworkflow.FragmentKey{Repo: "acme/publish", Version: "v1"}
	fragment := argoworkflow.Fragment{Key: key, SHA: testSHA, Source: fragmentDocument(`    - name: run
      container:
        image: ` + fragmentImage + `
        command: [/run/oberth/bin/oberth]
        args:
          - secretstore
          - exec
          - --dir=/run/oberth-secrets
          - --path=oberth/data/release/smuggled
          - --
          - ./publish.sh
`)}
	source := consumingDocument("    oberth.ci/secret-paths: oberth/data/release/approved\n", key.String())
	request := fragmentRequest(t, source, map[argoworkflow.FragmentKey]argoworkflow.Fragment{key: fragment})
	request.Trigger = periapsis.TriggerRelease
	request.ApprovedSecrets = map[string]bool{"oberth/data/release/approved": true}

	_, err := Build(testConfig(), request)
	if err == nil {
		t.Fatal("a fragment reached an undeclared secret path; resolution ran too late to be checked")
	}
	if !strings.Contains(err.Error(), "smuggled") {
		t.Fatalf("error does not name the path the fragment reached for: %v", err)
	}
}

func TestBuildAcceptsAFragmentUsingADeclaredAndApprovedPath(t *testing.T) {
	key := argoworkflow.FragmentKey{Repo: "acme/publish", Version: "v1"}
	fragment := argoworkflow.Fragment{Key: key, SHA: testSHA, Source: fragmentDocument(`    - name: run
      container:
        image: ` + fragmentImage + `
        command: [/run/oberth/bin/oberth]
        args:
          - secretstore
          - exec
          - --dir=/run/oberth-secrets
          - --path=oberth/data/release/approved
          - --
          - ./publish.sh
`)}
	source := consumingDocument("    oberth.ci/secret-paths: oberth/data/release/approved\n", key.String())
	request := fragmentRequest(t, source, map[argoworkflow.FragmentKey]argoworkflow.Fragment{key: fragment})
	request.Trigger = periapsis.TriggerRelease
	request.ApprovedSecrets = map[string]bool{"oberth/data/release/approved": true}

	if _, err := Build(testConfig(), request); err != nil {
		t.Fatalf("build: %v", err)
	}
}

func TestBuildRefusesAFragmentCarryingAConstructTheGateRejects(t *testing.T) {
	key := argoworkflow.FragmentKey{Repo: "acme/evil", Version: "v1"}
	fragment := argoworkflow.Fragment{Key: key, SHA: testSHA, Source: fragmentDocument(`    - name: run
      container:
        image: ` + fragmentImage + `
        command: [/bin/true]
        securityContext:
          privileged: true
`)}
	request := fragmentRequest(t, consumingDocument("    oberth.ci/size: S\n", key.String()),
		map[argoworkflow.FragmentKey]argoworkflow.Fragment{key: fragment})

	_, err := Build(testConfig(), request)
	if err == nil {
		t.Fatal("a fragment carried a construct the gate refuses and was admitted")
	}
	if !strings.Contains(err.Error(), "securityContext") {
		t.Fatalf("error does not name the refused construct: %v", err)
	}
}

func TestBuildRefusesAReferenceMissingFromTheFragmentMap(t *testing.T) {
	request := fragmentRequest(t, consumingDocument("    oberth.ci/size: S\n", "acme/absent@v9"),
		map[argoworkflow.FragmentKey]argoworkflow.Fragment{})

	_, err := Build(testConfig(), request)
	if err == nil {
		t.Fatal("a document referencing an unloaded fragment was admitted")
	}
	if !strings.Contains(err.Error(), "acme/absent@v9") {
		t.Fatalf("error does not name the missing fragment: %v", err)
	}
}

func TestBuildRefusesFragmentReferencesWithoutAPreLoadedMap(t *testing.T) {
	request := testRequest(periapsis.TriggerCI, consumingDocument("    oberth.ci/size: S\n", "acme/x@v1"))
	request.Fragments = nil

	if _, err := Build(testConfig(), request); err == nil {
		t.Fatal("a fragment reference resolved against a nil map")
	}
}

func TestBuildIsIdenticalWhenCalledTwice(t *testing.T) {
	key, fragment := plainFragment(t, "acme/maven-verify", "v3")
	request := fragmentRequest(t, consumingDocument("    oberth.ci/size: S\n", key.String()),
		map[argoworkflow.FragmentKey]argoworkflow.Fragment{key: fragment})

	first, err := Build(testConfig(), request)
	if err != nil {
		t.Fatalf("first build: %v", err)
	}
	second, err := Build(testConfig(), request)
	if err != nil {
		t.Fatalf("second build: %v", err)
	}
	encode := func(workflow *wfv1.Workflow) string {
		encoded, err := json.Marshal(workflow)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		return string(encoded)
	}
	if encode(first) != encode(second) {
		t.Fatal("auditSubmission builds and controller.Create builds again; the two produced different Workflows")
	}
}

func TestBuildWithoutFragmentsIsUnaffected(t *testing.T) {
	request := testRequest(periapsis.TriggerCI, greedyDocument)
	before, err := Build(testConfig(), request)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	request.Fragments = map[argoworkflow.FragmentKey]argoworkflow.Fragment{}
	after, err := Build(testConfig(), request)
	if err != nil {
		t.Fatalf("build with an empty fragment map: %v", err)
	}
	encode := func(workflow *wfv1.Workflow) string {
		encoded, err := json.Marshal(workflow)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		return string(encoded)
	}
	if encode(before) != encode(after) {
		t.Fatal("a document with no fragment references built differently once fragments existed")
	}
}

func TestBuildRefusesInliningPastTheTemplateCeiling(t *testing.T) {
	var templates strings.Builder
	for index := 0; index <= argoworkflow.MaxTemplates; index++ {
		name := "run"
		if index > 0 {
			name = fmt.Sprintf("filler%d", index)
		}
		fmt.Fprintf(&templates, "    - name: %s\n      container:\n        image: %s\n        command: [/bin/true]\n",
			name, fragmentImage)
	}
	key := argoworkflow.FragmentKey{Repo: "acme/huge", Version: "v1"}
	fragment := argoworkflow.Fragment{Key: key, SHA: testSHA, Source: fragmentDocument(templates.String())}
	request := fragmentRequest(t, consumingDocument("    oberth.ci/size: S\n", key.String()),
		map[argoworkflow.FragmentKey]argoworkflow.Fragment{key: fragment})

	if _, err := Build(testConfig(), request); err == nil {
		t.Fatal("inlining past the template ceiling was admitted")
	}
}

func TestBuildRecordsTheResolvedFragmentVersions(t *testing.T) {
	key, fragment := plainFragment(t, "acme/maven-verify", "v3")
	request := fragmentRequest(t, consumingDocument("    oberth.ci/size: S\n", key.String()),
		map[argoworkflow.FragmentKey]argoworkflow.Fragment{key: fragment})

	workflow, err := Build(testConfig(), request)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	recorded := workflow.Annotations[FragmentsAnnotation]
	if recorded == "" {
		t.Fatal("the submitted Workflow records no fragment versions, so the audit chain cannot either")
	}
	var lock argoworkflow.Lock
	if err := json.Unmarshal([]byte(recorded), &lock); err != nil {
		t.Fatalf("annotation is not decodable: %v", err)
	}
	if len(lock) != 1 {
		t.Fatalf("recorded %d fragments, want 1", len(lock))
	}
	if lock[0].Key != key {
		t.Fatalf("recorded key %+v, want %+v", lock[0].Key, key)
	}
	if lock[0].SHA != fragment.SHA {
		t.Fatalf("recorded SHA %q, want the commit the tag resolved to %q", lock[0].SHA, fragment.SHA)
	}
	if lock[0].Digest == "" {
		t.Fatal("recorded no document digest")
	}
}

func TestAMovedTagChangesTheRecordedSHA(t *testing.T) {
	key, fragment := plainFragment(t, "acme/maven-verify", "v3")
	source := consumingDocument("    oberth.ci/size: S\n", key.String())

	build := func(sha string) string {
		moved := fragment
		moved.SHA = sha
		request := fragmentRequest(t, source, map[argoworkflow.FragmentKey]argoworkflow.Fragment{key: moved})
		workflow, err := Build(testConfig(), request)
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		return workflow.Annotations[FragmentsAnnotation]
	}
	before := build(strings.Repeat("1", 40))
	after := build(strings.Repeat("2", 40))
	if before == after {
		t.Fatal("the same consuming commit recorded the same fragment SHA after the tag moved; a moved tag would be invisible")
	}
	if !strings.Contains(before, strings.Repeat("1", 40)) || !strings.Contains(after, strings.Repeat("2", 40)) {
		t.Fatalf("the recorded SHAs do not track the tag:\nbefore %s\nafter  %s", before, after)
	}
}

func TestBuildRecordsNothingWhenNoFragmentsAreUsed(t *testing.T) {
	workflow, err := Build(testConfig(), testRequest(periapsis.TriggerCI, greedyDocument))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if _, present := workflow.Annotations[FragmentsAnnotation]; present {
		t.Fatal("a pipeline using no fragments carries a fragments annotation")
	}
}
