package argoworkflow

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
)

const (
	releaseServiceAccount  = "oberth-argo-release"
	branchServiceAccount   = "oberth-argo-ci"
	executorServiceAccount = "oberth-argo-executor"
)

func testBranchIdentity() Identity {
	return Identity{
		Namespace:                    "oberth-argo",
		ServiceAccountName:           branchServiceAccount,
		ExecutorServiceAccountName:   executorServiceAccount,
		AutomountServiceAccountToken: false,
	}
}

func testReleaseIdentity() Identity {
	return Identity{
		Namespace:                    "oberth-argo",
		ServiceAccountName:           releaseServiceAccount,
		ExecutorServiceAccountName:   executorServiceAccount,
		AutomountServiceAccountToken: true,
	}
}

// expectedAccount reports which identity a given value path must carry: Argo's
// executor config names the executor identity, everything else names the
// pipeline's.
func expectedAccount(path string, identity Identity) string {
	if strings.Contains(path, ".Executor.") {
		return identity.ExecutorServiceAccountName
	}
	return identity.ServiceAccountName
}

// hostileDocument asks for the release ServiceAccount through every path a
// realistic YAML document can reach, at several nesting levels. It is the
// artifact a branch push would use to try to reach release credentials.
const hostileDocument = `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  activeDeadlineSeconds: 600
  serviceAccountName: oberth-argo-release
  automountServiceAccountToken: true
  executor:
    serviceAccountName: oberth-argo-release
  artifactGC:
    serviceAccountName: oberth-argo-release
  templateDefaults:
    serviceAccountName: oberth-argo-release
    automountServiceAccountToken: true
    executor:
      serviceAccountName: oberth-argo-release
  templates:
    - name: main
      serviceAccountName: oberth-argo-release
      automountServiceAccountToken: true
      executor:
        serviceAccountName: oberth-argo-release
      dag:
        tasks:
          - name: one
            inline:
              name: inline-one
              serviceAccountName: oberth-argo-release
              automountServiceAccountToken: true
              executor:
                serviceAccountName: oberth-argo-release
              container:
                image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
                command: [/bin/true]
          - name: two
            template: nested-steps
    - name: nested-steps
      serviceAccountName: oberth-argo-release
      steps:
        - - name: deep
            inline:
              name: inline-deep
              serviceAccountName: oberth-argo-release
              automountServiceAccountToken: true
              steps:
                - - name: deeper
                    inline:
                      name: inline-deeper
                      serviceAccountName: oberth-argo-release
                      automountServiceAccountToken: true
                      container:
                        image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
                        command: [/bin/true]
`

// TestBranchTierCannotReachTheReleaseServiceAccount is the load-bearing
// security test for the Argo path. A branch-tier submission must not carry the
// release ServiceAccount anywhere in the object graph, no matter what the
// repository-authored YAML asked for.
func TestBranchTierCannotReachTheReleaseServiceAccount(t *testing.T) {
	workflow, err := Decode([]byte(hostileDocument))
	if err != nil {
		t.Fatalf("decode hostile document: %v", err)
	}
	if err := Admit(workflow, Policy{}); err != nil {
		t.Fatalf("hostile document should pass structural admission (identity is forced, not rejected): %v", err)
	}
	branch := testBranchIdentity()
	if err := ForceIdentity(workflow, branch); err != nil {
		t.Fatalf("force branch identity: %v", err)
	}

	// A deep walk of the concrete object, not a field-by-field assertion, so a
	// schema path nobody thought of still fails the test.
	for _, found := range collectIdentityValues(t, reflect.ValueOf(workflow).Elem(), "workflow") {
		want := expectedAccount(found.path, branch)
		if found.serviceAccount != "" && found.serviceAccount != want {
			t.Errorf("%s carries ServiceAccount %q, want %q", found.path, found.serviceAccount, want)
		}
	}
	if strings.Contains(renderJSON(t, workflow), releaseServiceAccount) {
		t.Fatalf("submitted object still mentions %q anywhere in its encoding", releaseServiceAccount)
	}
}

// TestBranchTierExecutorIdentityIsSeparate proves Argo's own executor runs as a
// third identity, so the Kubernetes access the executor needs to report task
// results is never the pipeline's access.
func TestBranchTierExecutorIdentityIsSeparate(t *testing.T) {
	workflow, err := Decode([]byte(hostileDocument))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	branch := testBranchIdentity()
	if err := Prepare(workflow, Policy{}, branch); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	// Argo refuses a Workflow with no executor ServiceAccount when the step
	// container's token is withheld, so this must always be populated.
	if workflow.Spec.Executor == nil || workflow.Spec.Executor.ServiceAccountName != executorServiceAccount {
		t.Fatalf("spec.executor = %+v, want ServiceAccount %q", workflow.Spec.Executor, executorServiceAccount)
	}
	for index, template := range workflow.Spec.Templates {
		if template.Executor == nil {
			continue
		}
		if template.Executor.ServiceAccountName != executorServiceAccount {
			t.Errorf("template %d executor ServiceAccount = %q", index, template.Executor.ServiceAccountName)
		}
	}
}

// TestPrepareRefusesPodSpecPatchWhateverTheOrder proves the ordering hazard is
// closed: podSpecPatch is a raw strategic-merge string applied to the finished
// Pod spec, so no structural walk can neutralize it. Binding an identity into a
// document that carries one must fail rather than produce a patchable object.
func TestPrepareRefusesPodSpecPatchWhateverTheOrder(t *testing.T) {
	const patched = `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  activeDeadlineSeconds: 60
  podSpecPatch: '{"serviceAccountName":"oberth-argo-release"}'
  templates:
    - name: main
      container:
        image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
        command: [/bin/true]
`
	workflow, err := Decode([]byte(patched))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := Prepare(workflow, Policy{}, testBranchIdentity()); err == nil {
		t.Fatal("Prepare admitted a document carrying podSpecPatch")
	}
	// The backstop holds even when a caller skips Admit entirely.
	if err := ForceIdentity(workflow, testBranchIdentity()); err == nil {
		t.Fatal("ForceIdentity bound an identity into a document carrying podSpecPatch")
	}
}

func TestForceIdentityRefusesPodSpecPatchInsideInlineTemplates(t *testing.T) {
	const nested = `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  activeDeadlineSeconds: 60
  templates:
    - name: main
      dag:
        tasks:
          - name: sneaky
            inline:
              name: inline
              podSpecPatch: '{"hostPID":true}'
              container:
                image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
                command: [/bin/true]
`
	workflow, err := Decode([]byte(nested))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := ForceIdentity(workflow, testBranchIdentity()); err == nil {
		t.Fatal("ForceIdentity missed a nested podSpecPatch")
	}
}

func TestForceIdentityRefusesASharedExecutorIdentity(t *testing.T) {
	workflow, err := Decode([]byte(hostileDocument))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	identity := testBranchIdentity()
	identity.ExecutorServiceAccountName = identity.ServiceAccountName
	if err := ForceIdentity(workflow, identity); err == nil {
		t.Fatal("expected a shared pipeline/executor identity to be refused")
	}
}

// TestForceIdentityLeavesNoUnnormalizedIdentity is the exhaustive form of the
// test above. Rather than trusting a hand-written document to exercise every
// identity path, it builds a Workflow reflectively with a poison value at
// EVERY path the schema exposes, then proves none survives normalization.
//
// This is what makes the reflective normalizer trustworthy: the proof and the
// implementation derive the path set independently — the implementation from
// field names at run time, the test from the type graph.
func TestForceIdentityLeavesNoUnnormalizedIdentity(t *testing.T) {
	const poison = "attacker-chosen-account"
	workflow := &wfv1.Workflow{}
	populated := populateIdentityFields(reflect.ValueOf(workflow), poison, map[reflect.Type]bool{}, 0)
	if populated == 0 {
		t.Fatal("test fixture populated no identity fields; it would prove nothing")
	}
	t.Logf("populated %d identity fields with a poison value", populated)

	if err := ForceIdentity(workflow, testBranchIdentity()); err != nil {
		t.Fatalf("force: %v", err)
	}
	for _, found := range collectIdentityValues(t, reflect.ValueOf(workflow).Elem(), "workflow") {
		if found.serviceAccount == poison {
			t.Errorf("%s survived normalization with the poison value", found.path)
		}
		if found.automount != nil && *found.automount {
			t.Errorf("%s still automounts a token on the branch tier", found.path)
		}
	}
}

// TestBranchTierReceivesNoServiceAccountToken proves the second, independent
// layer: even with the right ServiceAccount forced, a branch pod is given no
// projected token, so an in-Pod Vault login cannot be attempted at all.
func TestBranchTierReceivesNoServiceAccountToken(t *testing.T) {
	workflow, err := Decode([]byte(hostileDocument))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	branch := testBranchIdentity()
	if err := ForceIdentity(workflow, branch); err != nil {
		t.Fatalf("force branch identity: %v", err)
	}
	for _, found := range collectIdentityValues(t, reflect.ValueOf(workflow).Elem(), "workflow") {
		if found.automount != nil && *found.automount {
			t.Errorf("%s automounts a ServiceAccount token on the branch tier", found.path)
		}
	}
}

func TestReleaseTierReceivesTheReleaseIdentity(t *testing.T) {
	workflow, err := Decode([]byte(hostileDocument))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	release := testReleaseIdentity()
	if err := ForceIdentity(workflow, release); err != nil {
		t.Fatalf("force release identity: %v", err)
	}
	if workflow.Namespace != "oberth-argo" {
		t.Fatalf("namespace = %q, want oberth-argo", workflow.Namespace)
	}
	seenAutomount := 0
	for _, found := range collectIdentityValues(t, reflect.ValueOf(workflow).Elem(), "workflow") {
		want := expectedAccount(found.path, release)
		if found.serviceAccount != "" && found.serviceAccount != want {
			t.Errorf("%s carries ServiceAccount %q, want %q", found.path, found.serviceAccount, want)
		}
		if found.automount != nil {
			seenAutomount++
			if !*found.automount {
				t.Errorf("%s does not automount a token on the release tier", found.path)
			}
		}
	}
	if seenAutomount == 0 {
		t.Fatal("no automountServiceAccountToken field was set on the release tier")
	}
}

// TestForceIdentityIgnoresARequestedIdentityEvenWhenItMatchesNothing proves the
// override is unconditional: a document naming an unrelated ServiceAccount is
// rewritten just the same, so nothing about the repository's request survives.
func TestForceIdentityIgnoresARequestedIdentityEvenWhenItMatchesNothing(t *testing.T) {
	workflow, err := Decode([]byte(strings.ReplaceAll(hostileDocument, releaseServiceAccount, "some-other-account")))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	branch := testBranchIdentity()
	if err := ForceIdentity(workflow, branch); err != nil {
		t.Fatalf("force: %v", err)
	}
	if strings.Contains(renderJSON(t, workflow), "some-other-account") {
		t.Fatal("the document's own ServiceAccount request survived normalization")
	}
}

// TestForceIdentityClearsSubmittedStatus proves a document cannot smuggle a
// second, unreviewed WorkflowSpec through status.storedWorkflowTemplateSpec.
func TestForceIdentityClearsSubmittedStatus(t *testing.T) {
	const withStatus = `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  activeDeadlineSeconds: 60
  templates:
    - name: main
      container:
        image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
        command: [/bin/true]
status:
  storedWorkflowTemplateSpec:
    entrypoint: main
    serviceAccountName: oberth-argo-release
`
	workflow, err := Decode([]byte(withStatus))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if workflow.Status.StoredWorkflowSpec == nil {
		t.Fatal("fixture did not decode a stored spec; the test would prove nothing")
	}
	if err := ForceIdentity(workflow, testBranchIdentity()); err != nil {
		t.Fatalf("force: %v", err)
	}
	if workflow.Status.StoredWorkflowSpec != nil {
		t.Fatal("submitted status retained a stored WorkflowSpec")
	}
}

func TestForceIdentityRejectsAnUnusableIdentity(t *testing.T) {
	for name, identity := range map[string]Identity{
		"no namespace":         {ServiceAccountName: branchServiceAccount, ExecutorServiceAccountName: executorServiceAccount},
		"no ServiceAccount":    {Namespace: "oberth-argo", ExecutorServiceAccountName: executorServiceAccount},
		"no executor account":  {Namespace: "oberth-argo", ServiceAccountName: branchServiceAccount},
		"bad namespace":        {Namespace: "Oberth_Argo", ServiceAccountName: branchServiceAccount, ExecutorServiceAccountName: executorServiceAccount},
		"bad ServiceAccount":   {Namespace: "oberth-argo", ServiceAccountName: "Not A Name", ExecutorServiceAccountName: executorServiceAccount},
		"bad executor account": {Namespace: "oberth-argo", ServiceAccountName: branchServiceAccount, ExecutorServiceAccountName: "Not A Name"},
	} {
		t.Run(name, func(t *testing.T) {
			workflow, err := Decode([]byte(hostileDocument))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if err := ForceIdentity(workflow, identity); err == nil {
				t.Fatal("expected an unusable identity to be refused")
			}
		})
	}
}

func TestForceIdentityRefusesUnboundedNesting(t *testing.T) {
	template := &wfv1.Template{Name: "leaf"}
	for range MaxIdentityWalkDepth + 4 {
		template = &wfv1.Template{
			Steps: []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{Name: "s", Inline: template}}}},
		}
	}
	workflow := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Templates: []wfv1.Template{*template}}}
	err := ForceIdentity(workflow, testBranchIdentity())
	if err == nil {
		t.Fatal("expected deeply nested inline templates to be refused")
	}
}

// TestIdentityFieldPathsAreKnown pins the schema's identity attack surface. It
// does not gate correctness — the normalizer is name-based and therefore
// covers new paths automatically — but an Argo upgrade that widens the surface
// should be a visible diff a human reviews, not a silent change.
func TestIdentityFieldPathsAreKnown(t *testing.T) {
	known := []string{
		"WorkflowSpec.Arguments.Artifacts.ArtifactGC.ServiceAccountName",
		"WorkflowSpec.ArtifactGC.ArtifactGC.ServiceAccountName",
		"WorkflowSpec.AutomountServiceAccountToken",
		"WorkflowSpec.Executor.ServiceAccountName",
		"WorkflowSpec.Hooks.Arguments.Artifacts.ArtifactGC.ServiceAccountName",
		"WorkflowSpec.ServiceAccountName",
		"WorkflowSpec.TemplateDefaults.AutomountServiceAccountToken",
		"WorkflowSpec.TemplateDefaults.DAG.Tasks.Arguments.Artifacts.ArtifactGC.ServiceAccountName",
		"WorkflowSpec.TemplateDefaults.DAG.Tasks.Hooks.Arguments.Artifacts.ArtifactGC.ServiceAccountName",
		"WorkflowSpec.TemplateDefaults.Data.Source.ArtifactPaths.Artifact.ArtifactGC.ServiceAccountName",
		"WorkflowSpec.TemplateDefaults.Executor.ServiceAccountName",
		"WorkflowSpec.TemplateDefaults.Inputs.Artifacts.ArtifactGC.ServiceAccountName",
		"WorkflowSpec.TemplateDefaults.Outputs.Artifacts.ArtifactGC.ServiceAccountName",
		"WorkflowSpec.TemplateDefaults.Resource.ManifestFrom.Artifact.ArtifactGC.ServiceAccountName",
		"WorkflowSpec.TemplateDefaults.ServiceAccountName",
		"WorkflowSpec.TemplateDefaults.Steps.Steps.Arguments.Artifacts.ArtifactGC.ServiceAccountName",
		"WorkflowSpec.TemplateDefaults.Steps.Steps.Hooks.Arguments.Artifacts.ArtifactGC.ServiceAccountName",
		"WorkflowSpec.Templates.AutomountServiceAccountToken",
		"WorkflowSpec.Templates.DAG.Tasks.Arguments.Artifacts.ArtifactGC.ServiceAccountName",
		"WorkflowSpec.Templates.DAG.Tasks.Hooks.Arguments.Artifacts.ArtifactGC.ServiceAccountName",
		"WorkflowSpec.Templates.Data.Source.ArtifactPaths.Artifact.ArtifactGC.ServiceAccountName",
		"WorkflowSpec.Templates.Executor.ServiceAccountName",
		"WorkflowSpec.Templates.Inputs.Artifacts.ArtifactGC.ServiceAccountName",
		"WorkflowSpec.Templates.Outputs.Artifacts.ArtifactGC.ServiceAccountName",
		"WorkflowSpec.Templates.Resource.ManifestFrom.Artifact.ArtifactGC.ServiceAccountName",
		"WorkflowSpec.Templates.ServiceAccountName",
		"WorkflowSpec.Templates.Steps.Steps.Arguments.Artifacts.ArtifactGC.ServiceAccountName",
		"WorkflowSpec.Templates.Steps.Steps.Hooks.Arguments.Artifacts.ArtifactGC.ServiceAccountName",
	}
	found := IdentityFieldPaths()
	slices.Sort(found)
	slices.Sort(known)
	for _, path := range found {
		if !slices.Contains(known, path) {
			t.Errorf("upstream schema grew an identity path: %s\n"+
				"the reflective normalizer already covers it; add it to this list after reviewing", path)
		}
	}
	for _, path := range known {
		if !slices.Contains(found, path) {
			t.Errorf("known identity path %s no longer exists upstream; remove it from this list", path)
		}
	}
}

// populateIdentityFields writes a poison ServiceAccount and an automounted
// token at every identity path reachable in the type graph, allocating
// pointers, slices, and maps along the way. It returns how many fields it set.
func populateIdentityFields(value reflect.Value, poison string, visiting map[reflect.Type]bool, depth int) int {
	if depth > 12 {
		return 0
	}
	switch value.Kind() {
	case reflect.Pointer:
		if !carriesIdentity(value.Type(), map[reflect.Type]bool{}, 0) {
			return 0
		}
		if value.IsNil() {
			if !value.CanSet() {
				return 0
			}
			value.Set(reflect.New(value.Type().Elem()))
		}
		return populateIdentityFields(value.Elem(), poison, visiting, depth+1)
	case reflect.Slice:
		if !carriesIdentity(value.Type(), map[reflect.Type]bool{}, 0) {
			return 0
		}
		if !value.CanSet() {
			return 0
		}
		value.Set(reflect.MakeSlice(value.Type(), 1, 1))
		return populateIdentityFields(value.Index(0), poison, visiting, depth+1)
	case reflect.Map:
		if !carriesIdentity(value.Type(), map[reflect.Type]bool{}, 0) || !value.CanSet() {
			return 0
		}
		value.Set(reflect.MakeMap(value.Type()))
		entry := reflect.New(value.Type().Elem()).Elem()
		count := populateIdentityFields(entry, poison, visiting, depth+1)
		value.SetMapIndex(reflect.ValueOf("populated").Convert(value.Type().Key()), entry)
		return count
	case reflect.Struct:
		if visiting[value.Type()] {
			return 0
		}
		visiting[value.Type()] = true
		defer delete(visiting, value.Type())
		count := 0
		for index := range value.NumField() {
			definition := value.Type().Field(index)
			if !definition.IsExported() {
				continue
			}
			field := value.Field(index)
			switch definition.Name {
			case serviceAccountField:
				if field.Kind() == reflect.String && field.CanSet() {
					field.SetString(poison)
					count++
					continue
				}
			case automountField:
				if setAutomount(field, true) {
					count++
					continue
				}
			}
			count += populateIdentityFields(field, poison, visiting, depth+1)
		}
		return count
	default:
		return 0
	}
}

type identityValue struct {
	path           string
	serviceAccount string
	automount      *bool
}

// collectIdentityValues walks a concrete decoded object (not the type graph)
// and reports every ServiceAccountName and AutomountServiceAccountToken value
// actually present, with its value path.
func collectIdentityValues(t *testing.T, value reflect.Value, path string) []identityValue {
	t.Helper()
	return walkIdentityValues(value, path, 0)
}

func walkIdentityValues(value reflect.Value, path string, depth int) []identityValue {
	if depth > 128 {
		return nil
	}
	switch value.Kind() {
	case reflect.Pointer, reflect.Interface:
		if value.IsNil() {
			return nil
		}
		return walkIdentityValues(value.Elem(), path, depth+1)
	case reflect.Slice, reflect.Array:
		var found []identityValue
		for index := range value.Len() {
			found = append(found, walkIdentityValues(value.Index(index), fmt.Sprintf("%s[%d]", path, index), depth+1)...)
		}
		return found
	case reflect.Map:
		var found []identityValue
		for _, key := range value.MapKeys() {
			found = append(found, walkIdentityValues(value.MapIndex(key), fmt.Sprintf("%s[%v]", path, key), depth+1)...)
		}
		return found
	case reflect.Struct:
		var found []identityValue
		for index := range value.NumField() {
			field := value.Type().Field(index)
			if !field.IsExported() {
				continue
			}
			next := path + "." + field.Name
			switch field.Name {
			case serviceAccountField:
				found = append(found, identityValue{path: next, serviceAccount: value.Field(index).String()})
			case automountField:
				entry := identityValue{path: next}
				automount := value.Field(index)
				if automount.Kind() == reflect.Pointer && !automount.IsNil() {
					decoded := automount.Elem().Bool()
					entry.automount = &decoded
				} else if automount.Kind() == reflect.Bool {
					decoded := automount.Bool()
					entry.automount = &decoded
				}
				found = append(found, entry)
			default:
				found = append(found, walkIdentityValues(value.Field(index), next, depth+1)...)
			}
		}
		return found
	default:
		return nil
	}
}
