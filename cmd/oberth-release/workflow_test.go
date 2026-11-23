package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

// walkFile is the VM install walk. It lives outside .oberth/, so no push,
// promotion, tag or schedule selects it; validation reads it as CI.
const walkFile = "hack/walk-vm.yaml"

func ownWorkflow(t *testing.T, name string) []byte {
	t.Helper()
	path := "../../.oberth/" + name + ".yaml"
	if name == "walk" {
		path = "../../" + walkFile
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return body
}
func ownRequest(document []byte, name string, trigger model.RunTrigger) admissioncontract.PrepareRequest {
	sha := strings.Repeat("a", 40)
	revision := sourcecontract.Revision{Repository: "forge/oberthci/oberth", Commit: sha}
	file, resources := ".oberth/"+name+".yaml", ""
	if name == "walk" {
		file, resources = walkFile, "vm-executor"
	}
	return admissioncontract.PrepareRequest{Mode: admissioncontract.ModeSubmit, Document: document, Repository: revision.Repository, RunID: "own-run", Ref: "refs/heads/main", ObjectSHA: sha, CommitSHA: sha, Trigger: trigger, SelectedFile: file, FileBlob: sha,
		SourceReceipt:   sourcecontract.Receipt{Version: 1, Revision: revision, ObjectSHA: sha, File: file, Blob: sha, DocumentDigest: sourcecontract.Digest(document)},
		IdentityRequest: admissioncontract.IdentityRequest{Namespace: "ci", PipelineAccount: "ci-step", ResourceAccount: resources}}
}
func ownHooks(t *testing.T, request admissioncontract.PrepareRequest) admissioncontract.PrepareHooks {
	return admissioncontract.PrepareHooks{
		ReleaseFacts: func(context.Context) (admissioncontract.ReleaseFacts, error) {
			return admissioncontract.ReleaseFacts{Version: "v1.2.3", TestedCommit: request.CommitSHA, DefaultCommit: request.CommitSHA, BuildStatus: "passed"}, nil
		},
		LoginAudience: chartLoginAudience(t), VerifyIdentities: func(_ context.Context, r admissioncontract.IdentityRequest) (admissioncontract.IdentityEvidence, error) {
			e := admissioncontract.IdentityEvidence{Namespace: r.Namespace, Pipeline: admissioncontract.AccountEvidence{Name: r.PipelineAccount, UID: "pipeline"}, State: admissioncontract.StateVerified}
			if r.ResourceAccount != "" {
				e.Resource = &admissioncontract.AccountEvidence{Name: r.ResourceAccount, UID: "resource"}
			}
			return e, nil
		},
		BindSource: func(_ context.Context, d sourcecontract.Descriptor) (sourcecontract.Binding, error) {
			return sourcecontract.Binding{Descriptor: d, RepositoryID: 1, Revision: request.SourceReceipt.Revision}, nil
		},
	}
}

// ownPolicy is what any repository's build and release get: claims, no capability.
func ownPolicy() argoworkflow.Policy {
	return argoworkflow.Policy{ServerNamespace: "oberth", ArgoNamespace: "argo", PVCClaims: map[string]bool{"build-cache": true, "release-cache": true}}
}

// walkPolicy adds the VM capability only the walk asks for.
func walkPolicy() argoworkflow.Policy {
	p := ownPolicy()
	p.Capability = &argoworkflow.Capability{Namespace: "oberth-walk", ServiceAccount: "vm-executor", ResourceKinds: []argoworkflow.ResourceKind{{APIVersion: "kubevirt.io/v1", Kind: "VirtualMachineInstance", Actions: []string{"create", "delete"}}}}
	return p
}
func prepareOwn(t *testing.T, name string, trigger model.RunTrigger) (*admissioncontract.PreparedAdmission, error) {
	t.Helper()
	r := ownRequest(ownWorkflow(t, name), name, trigger)
	p := ownPolicy()
	switch name {
	case "build":
		r.Grants = policyGrants(r)
	case "release":
		r.Grants = releaseGrants(r)
		p.Release = true
	case "walk":
		p = walkPolicy()
	}
	return argoworkflow.Prepare(t.Context(), r, p, ownHooks(t, r))
}
func TestOwnPipelinesAdmit(t *testing.T) {
	for _, tc := range []struct {
		name    string
		trigger model.RunTrigger
	}{{"build", model.TriggerBranch}, {"build", model.TriggerPromotion}, {"release", model.TriggerTag}, {"walk", model.TriggerBranch}} {
		p, err := prepareOwn(t, tc.name, tc.trigger)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range p.View().Plan.Entries {
			if len(entry.Name) > 40 {
				t.Fatal("long name", entry.Name)
			}
		}
	}
}

func TestOwnReleaseAdmits(t *testing.T) {
	p, err := prepareOwn(t, "release", model.TriggerTag)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.View().Plan.Entries) != 7 {
		t.Fatal(p.View().Plan)
	}
	credentials := map[string]bool{"release-files-signed": true, "release-images-pushed": true, "release-published-immutable": true, "latest-advanced-monotonic": true}
	for _, entry := range p.View().Plan.Entries {
		if entry.CredentialIntent != credentials[entry.Name] {
			t.Fatal("credential intent differs", entry.Name)
		}
	}
}
func TestOwnPlanStaysSmall(t *testing.T) {
	for _, name := range []string{"build", "release", "walk"} {
		assertSmallDAG(t, name)
	}
	for _, tc := range []struct {
		name string
		want []string
	}{
		{"build", []string{"prepare-base", "go-tests-pass-with-race", "release-bytes-build", "lint-and-guards-pass"}},
		{"walk", []string{"prepare-base", "release-bytes-build", "walk-vm-key-generated", "walk-vm-boots", "first-run-walk-passes", "walk-vm-deleted"}},
	} {
		p, err := prepareOwn(t, tc.name, model.TriggerPromotion)
		if err != nil {
			t.Fatal(tc.name, err)
		}
		if !p.View().Plan.Exact || len(p.View().Plan.Entries) != len(tc.want) {
			t.Fatalf("%s plan: %+v", tc.name, p.View().Plan)
		}
		for i, e := range p.View().Plan.Entries {
			if e.Name != tc.want[i] {
				t.Fatalf("%s step %d: %s", tc.name, i, e.Name)
			}
		}
	}
}

// The promotion gate asks for what any repository's would: no resource
// template, no VM, no capability, no resource account.
func TestPromotionGateHasNoResourceOrVMStep(t *testing.T) {
	p, err := prepareOwn(t, "build", model.TriggerPromotion)
	if err != nil {
		t.Fatal("promotion gate needs more than claims and its names grant", err)
	}
	for _, e := range p.View().Plan.Entries {
		if strings.Contains(e.Name, "vm") || strings.Contains(e.Name, "walk") {
			t.Fatal("VM step in the promotion gate", e.Name)
		}
	}
	assertGateDeclaresNoVM(t)
	assertVMStepNeedsCapability(t)
}

func assertGateDeclaresNoVM(t *testing.T) {
	t.Helper()
	wf, err := argoworkflow.Decode(ownWorkflow(t, "build"))
	if err != nil {
		t.Fatal(err)
	}
	if wf.Spec.OnExit != "" {
		t.Fatal("promotion gate has an exit handler", wf.Spec.OnExit)
	}
	for _, template := range wf.Spec.Templates {
		if template.Resource != nil {
			t.Fatal("resource template in the promotion gate", template.Name)
		}
	}
	for _, task := range wf.GetTemplateByName("build").DAG.Tasks {
		if task.When != "" {
			t.Fatal("promotion runs work a branch does not", task.Name)
		}
	}
}

// assertVMStepNeedsCapability is the control: the walk's VM step put back
// into the gate needs the capability again.
func assertVMStepNeedsCapability(t *testing.T) {
	t.Helper()
	walk, err := argoworkflow.Decode(ownWorkflow(t, "walk"))
	if err != nil {
		t.Fatal(err)
	}
	withVM := rewriteWorkflow(t, ownWorkflow(t, "build"), func(w *wfv1.Workflow) {
		w.Spec.Templates = append(w.Spec.Templates, *walk.GetTemplateByName("delete-vm"))
		dag := w.GetTemplateByName("build").DAG
		dag.Tasks = append(dag.Tasks, wfv1.DAGTask{Name: "walk-vm-deleted", Template: "delete-vm"})
	})
	r := ownRequest(withVM, "build", model.TriggerPromotion)
	r.Grants = policyGrants(r)
	if _, err := argoworkflow.Prepare(t.Context(), r, ownPolicy(), ownHooks(t, r)); err == nil {
		t.Fatal("VM step admitted without a capability")
	}
}
func TestWalkVMManifestAdmits(t *testing.T) {
	r := ownRequest(ownWorkflow(t, "walk"), "walk", model.TriggerBranch)
	if _, err := argoworkflow.Prepare(t.Context(), r, ownPolicy(), ownHooks(t, r)); err == nil {
		t.Fatal("VM without capability accepted")
	}
	p := walkPolicy()
	p.Release = true
	r.Trigger = model.TriggerTag
	if _, err := argoworkflow.Prepare(t.Context(), r, p, ownHooks(t, r)); err == nil {
		t.Fatal("VM in release accepted")
	}
	bad := rewriteWorkflow(t, r.Document, func(w *wfv1.Workflow) {
		w.GetTemplateByName("vm").Resource.Manifest = strings.ReplaceAll(w.GetTemplateByName("vm").Resource.Manifest, "namespace: oberth-walk", "namespace: other")
	})
	r = ownRequest(bad, "walk", model.TriggerBranch)
	if _, err := argoworkflow.Prepare(t.Context(), r, walkPolicy(), ownHooks(t, r)); err == nil {
		t.Fatal("VM in ungranted namespace accepted")
	}
}
func TestRefsAreQuotedArguments(t *testing.T) {
	for _, name := range []string{"build", "release", "walk"} {
		t.Run(name, func(t *testing.T) { assertQuotedRefs(t, name) })
	}
}
func assertQuotedRefs(t *testing.T, name string) {
	t.Helper()
	wf, err := argoworkflow.Decode(ownWorkflow(t, name))
	if err != nil {
		t.Fatal(err)
	}
	for _, template := range wf.Spec.Templates {
		if template.Container != nil {
			for _, arg := range template.Container.Args {
				if strings.Contains(arg, "{{") {
					t.Fatal("template in shell text", arg)
				}
			}
		}
		if template.Script != nil && strings.Contains(template.Script.Source, "{{") {
			t.Fatal("template in script")
		}
	}
}

func rewriteWorkflow(t *testing.T, body []byte, change func(*wfv1.Workflow)) []byte {
	t.Helper()
	wf, err := argoworkflow.Decode(body)
	if err != nil {
		t.Fatal(err)
	}
	change(wf)
	raw, err := json.Marshal(wf)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestOwnGoSemaphoreAndBranchSelection(t *testing.T) {
	wf, err := argoworkflow.Decode(ownWorkflow(t, "build"))
	if err != nil {
		t.Fatal(err)
	}
	goTemplate := wf.GetTemplateByName("go")
	if goTemplate == nil || goTemplate.Synchronization == nil || len(goTemplate.Synchronization.Semaphores) != 1 {
		t.Fatal("heavy Go steps have no semaphore")
	}
	semaphore := goTemplate.Synchronization.Semaphores[0].ConfigMapKeyRef
	if semaphore == nil || semaphore.Name != "oberth-sync-limits" || semaphore.Key != "go" {
		t.Fatal("semaphore differs")
	}
	unconditional := 0
	for _, task := range wf.GetTemplateByName("build").DAG.Tasks {
		if task.When == "" {
			unconditional++
		}
	}
	if unconditional != 4 {
		t.Fatal("branch has extra work", unconditional)
	}
}
func TestOwnAuthoredPodFloor(t *testing.T) {
	wf, err := argoworkflow.Decode(ownWorkflow(t, "build"))
	if err != nil {
		t.Fatal(err)
	}
	if wf.Spec.SecurityContext == nil || wf.Spec.SecurityContext.RunAsNonRoot == nil || !*wf.Spec.SecurityContext.RunAsNonRoot {
		t.Fatal("pod floor missing")
	}
}
func TestOwnAuthoredContainerFloor(t *testing.T) {
	wf, err := argoworkflow.Decode(ownWorkflow(t, "build"))
	if err != nil {
		t.Fatal(err)
	}
	security := wf.Spec.TemplateDefaults.Container.SecurityContext
	if security == nil || security.AllowPrivilegeEscalation == nil || *security.AllowPrivilegeEscalation || len(security.Capabilities.Drop) != 1 || security.Capabilities.Drop[0] != "ALL" {
		t.Fatal("container floor missing")
	}
}

func releaseGrants(r admissioncontract.PrepareRequest) []admissioncontract.GrantEvidence {
	var grants []admissioncontract.GrantEvidence
	for _, item := range [][4]string{
		{"release-files-signed", "secret", "oberth/data/release/signing", ""},
		{"release-images-pushed", "secret", "oberth/data/release/registry/config.json", ""},
		{"release-published-immutable", "secret", "oberth/data/release/github-token", ""},
		{"release-published-immutable", "configmap", "release-config", ""},
		{"release-verifies-from-clean-pod", "configmap", "release-config", ""},
		{"latest-advanced-monotonic", "secret", "oberth/data/release/github-token", ""},
		{"latest-advanced-monotonic", "configmap", "release-config", ""},
	} {
		grants = append(grants, admissioncontract.GrantEvidence{Repository: r.Repository, File: r.SelectedFile, Step: item[0], Kind: item[1], Path: item[2], Audience: item[3], State: admissioncontract.StateVerified, Revision: "1"})
	}
	return grants
}

// Covers: REQ-C13-001
func TestExactShortLivedPublicationAuthorityContract(t *testing.T) {
	r := ownRequest(ownWorkflow(t, "release"), "release", model.TriggerTag)
	r.Grants = releaseGrants(r)
	p := ownPolicy()
	p.Release = true
	if _, err := argoworkflow.Prepare(t.Context(), r, p, ownHooks(t, r)); err != nil {
		t.Fatal(err)
	}
	for index := range r.Grants {
		for _, field := range []string{"repository", "file", "step"} {
			bad := r.Clone()
			switch field {
			case "repository":
				bad.Grants[index].Repository = "forge/other/repository"
			case "file":
				bad.Grants[index].File = ".oberth/other.yaml"
			case "step":
				bad.Grants[index].Step = "other"
			}
			if _, err := argoworkflow.Prepare(t.Context(), bad, p, ownHooks(t, bad)); err == nil {
				t.Fatal("changed exact grant accepted", index, field)
			}
		}
	}
}
func TestReleaseCredentialVolumes(t *testing.T) {
	r := ownRequest(ownWorkflow(t, "release"), "release", model.TriggerTag)
	wf, err := argoworkflow.Decode(r.Document)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"release-published-immutable", "latest-advanced-monotonic"} {
		template := wf.GetTemplateByName(name)
		if template.Metadata.Annotations[argoworkflow.SecretPathsAnnotation] != "oberth/data/release/github-token" || len(template.Volumes) != 0 {
			t.Fatal("publisher authority is not the granted GitHub token alone", name)
		}
	}
	verify := wf.GetTemplateByName("release-verifies-from-clean-pod")
	if verify.Metadata.Annotations[argoworkflow.SecretPathsAnnotation] != "" {
		t.Fatal("verify has a token")
	}
	for _, volume := range verify.Volumes {
		if volume.Projected != nil {
			t.Fatal("verify has a token")
		}
	}
}

// Only one publication or latest-marking step runs at a time.
func TestReleasePublicationStepsShareOneSemaphore(t *testing.T) {
	wf, err := argoworkflow.Decode(ownWorkflow(t, "release"))
	if err != nil {
		t.Fatal(err)
	}
	limits := pipelineResources(t, buildResourcesFile)["ConfigMap/oberth-sync-limits"]
	if limits.Data["release-publication"] != "1" {
		t.Fatal("publication semaphore must have capacity one")
	}
	for _, template := range wf.Spec.Templates {
		locked := false
		if s := template.Synchronization; s != nil && len(s.Semaphores) == 1 {
			sem := s.Semaphores[0]
			locked = sem.Namespace == "" && sem.Database == nil && sem.ConfigMapKeyRef != nil && sem.ConfigMapKeyRef.Name == "oberth-sync-limits" && sem.ConfigMapKeyRef.Key == "release-publication"
		}
		if locked != (template.Name == "release-published-immutable" || template.Name == "latest-advanced-monotonic") {
			t.Fatal("publication semaphore differs", template.Name)
		}
	}
}

func assertSmallDAG(t *testing.T, name string) {
	t.Helper()
	body := string(ownWorkflow(t, name))
	start := strings.Index(body, "      dag:\n")
	if start < 0 {
		t.Fatal("DAG missing")
	}
	tail := body[start:]
	end := strings.Index(tail, "\n    - name:")
	if end < 0 || strings.Count(tail[:end], "\n") > 40 {
		t.Fatal("DAG exceeds one screen")
	}
}

func TestVMDoesNotUseCrossNamespaceOwnerReference(t *testing.T) {
	workflow, err := argoworkflow.Decode(ownWorkflow(t, "walk"))
	if err != nil {
		t.Fatal(err)
	}
	if workflow.GetTemplateByName("vm").Resource.SetOwnerReference {
		t.Fatal("VM namespace differs from the protected workflow namespace")
	}
	if workflow.Spec.OnExit != "cleanup" {
		t.Fatal("VM has no native onExit cleanup")
	}
}

func TestSigningUsesTheBuiltCLI(t *testing.T) {
	workflow, err := argoworkflow.Decode(ownWorkflow(t, "release"))
	if err != nil {
		t.Fatal(err)
	}
	if workflow.GetTemplateByName("release-files-signed").Container.Command[0] != "/work/shared/oberth" {
		t.Fatal("signing depends on an unrelated installed CLI")
	}
	t.Run("native-login", func(t *testing.T) { assertNoAuthoredStoreLogin(t, workflow.GetTemplateByName("release-files-signed")) })
	t.Run("arguments", func(t *testing.T) { assertSigningCLI(t, workflow.GetTemplateByName("release-files-signed").Container) })
	if !strings.Contains(workflow.GetTemplateByName("release-bytes-build").Container.Args[0], `cp "$RELEASE_DIR/oberth-linux-amd64" "$RELEASE_SHARED/oberth"`) {
		t.Fatal("built CLI was not handed to signing")
	}
}
