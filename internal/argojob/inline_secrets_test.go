package argojob_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/argojob/fake"
	"github.com/oberthci/oberth/internal/identityname"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/runtimeconfig"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	core "k8s.io/api/core/v1"
)

func TestInlineLeafCannotInheritNamedSecretGrant(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"container", "script", "container-set"} {
		for _, assist := range []bool{false, true} {
			name := kind + map[bool]string{false: "/assist-off", true: "/assist-on"}[assist]
			t.Run(name, func(t *testing.T) {
				view := prepareInlineSecretCase(t, kind, assist, false)
				named := view.Workflow.Spec.Templates[1]
				inline := *view.Workflow.Spec.Templates[0].DAG.Tasks[1].Inline
				if inline.ServiceAccountName != account(t, identityname.PipelineCI, "") {
					t.Fatal("inline leaf inherited a named store account")
				}
				assertNoStoreInputs(t, inline)
				assertGrantedLeaf(t, view, named, account(t, identityname.StepCI, "private"))
			})
		}
	}
}

func TestInlineLeafReceivesOnlyWildcardSecretGrant(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"container", "script", "container-set"} {
		t.Run(kind, func(t *testing.T) {
			view := prepareInlineSecretCase(t, kind, true, true)
			inline := *view.Workflow.Spec.Templates[0].DAG.Tasks[1].Inline
			assertGrantedLeaf(t, view, inline, account(t, identityname.StepCI, "*"))
		})
	}
}

func prepareInlineSecretCase(t *testing.T, kind string, assist, wildcard bool) admissioncontract.PreparedView {
	t.Helper()
	w := secretWorkflow()
	w.Spec.Templates[1] = ordinarySecretLeaf(kind)
	w.Spec.Templates[1].Metadata.Annotations = map[string]string{argoworkflow.SecretPathsAnnotation: "public/key"}
	inline := ordinarySecretLeaf(kind)
	if wildcard {
		inline.Metadata.Annotations = map[string]string{argoworkflow.SecretPathsAnnotation: "public/key"}
	}
	w.Spec.Templates[0].DAG.Tasks[1] = wf.DAGTask{Name: "ungranted", Inline: &inline}
	document, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	sha, blob := strings.Repeat("a", 40), strings.Repeat("b", 40)
	repo, file := "forge/org/repo", ".oberth/build.yaml"
	step := "private"
	if wildcard {
		step = "*"
	}
	request := admissioncontract.PrepareRequest{Mode: admissioncontract.ModeSubmit, Repository: repo, RunID: "run-0000000000000001", Ref: "refs/heads/topic", ObjectSHA: sha, CommitSHA: sha, Trigger: model.TriggerBranch, SelectedFile: file, FileBlob: blob, Document: document,
		SourceReceipt:   sourcecontract.Receipt{Version: 1, Revision: sourcecontract.Revision{Repository: repo, Commit: sha}, ObjectSHA: sha, File: file, Blob: blob, DocumentDigest: sourcecontract.Digest(document)},
		IdentityRequest: admissioncontract.IdentityRequest{Namespace: "ci", PipelineAccount: account(t, identityname.PipelineCI, "")},
		Grants:          []admissioncontract.GrantEvidence{{Repository: repo, File: file, Step: step, Kind: "secret", Path: "public/key", Revision: "1", State: admissioncontract.StateVerified}}}
	accounts := map[string]string{request.IdentityRequest.PipelineAccount: "pipeline-uid", account(t, identityname.StepCI, step): "grant-uid"}
	executor := argojob.New(argojob.Config{SecretStore: secretDelivery(), Accounts: fake.Accounts{Names: accounts}})
	preparation := nativebinding.Preparation{Repository: repo, File: file, Assist: assist, HoldTTL: time.Minute, HelperImage: secretHelperTestImage}
	prepared, err := argoworkflow.Prepare(t.Context(), request, argoworkflow.Policy{}, executor.Hooks(preparation))
	if err != nil {
		t.Fatal(err)
	}
	return prepared.View()
}

func ordinarySecretLeaf(kind string) wf.Template {
	container := core.Container{Name: "main", Image: "build:1", Command: []string{"true"}}
	leaf := wf.Template{Name: "private"}
	switch kind {
	case "container":
		leaf.Container = &container
	case "script":
		container.Command = []string{"sh"}
		leaf.Script = &wf.ScriptTemplate{Container: container, Source: "true"}
	case "container-set":
		leaf.ContainerSet = &wf.ContainerSetTemplate{Containers: []wf.ContainerNode{{Container: container}}}
	}
	return leaf
}

func assertNoStoreInputs(t *testing.T, leaf wf.Template) {
	t.Helper()
	names := []string{runtimeconfig.SecretConfigEnv, runtimeconfig.SecretPathsEnv, runtimeconfig.SecretTokenEnv, runtimeconfig.SecretScratchEnv}
	for _, container := range secretLeafContainers(leaf) {
		for _, env := range container.Env {
			if slices.Contains(names, env.Name) {
				t.Fatal("inline leaf inherited a named store environment")
			}
		}
		for _, mount := range container.VolumeMounts {
			if mount.Name == "oberth-store-token" || mount.Name == "oberth-secret-scratch" {
				t.Fatal("inline leaf inherited a named store mount")
			}
		}
	}
	for _, volume := range leaf.Volumes {
		if volume.Name == "oberth-store-token" || volume.Name == "oberth-secret-scratch" {
			t.Fatal("inline leaf inherited a named store volume")
		}
	}
}

func assertGrantedLeaf(t *testing.T, view admissioncontract.PreparedView, leaf wf.Template, want string) {
	t.Helper()
	if leaf.ServiceAccountName != want {
		t.Fatal("granted leaf lost its assigned store account")
	}
	for _, container := range secretLeafContainers(leaf) {
		_, paths := secretEnvironment(t, container.Env)
		if len(paths) != 1 || paths[0] != "public/key" {
			t.Fatal("granted leaf received incorrect store paths")
		}
		if len(container.VolumeMounts) != 3 || len(leaf.Volumes) != 3 {
			t.Fatal("granted leaf lost store mounts")
		}
		checkSecretMounts(t, wf.Template{Volumes: leaf.Volumes, Container: &container})
	}
	assertGrantedPlan(t, view.Plan)
}

func assertGrantedPlan(t *testing.T, plan admissioncontract.Plan) {
	t.Helper()
	found := false
	for _, entry := range plan.Entries {
		if !entry.CredentialIntent {
			continue
		}
		found = true
		if entry.HoldEligible || entry.HoldArmed {
			t.Fatal("granted leaf became eligible for a hold")
		}
	}
	if !found {
		t.Fatal("granted leaf is missing from the admitted plan")
	}
}

func secretLeafContainers(leaf wf.Template) []core.Container {
	if leaf.Container != nil {
		return []core.Container{*leaf.Container}
	}
	if leaf.Script != nil {
		return []core.Container{leaf.Script.Container}
	}
	var containers []core.Container
	for _, node := range leaf.ContainerSet.Containers {
		containers = append(containers, node.Container)
	}
	return containers
}
