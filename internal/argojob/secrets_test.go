package argojob_test

import (
	"encoding/json"
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
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func secretDelivery() runtimeconfig.SecretDelivery {
	return runtimeconfig.SecretDelivery{Address: "https://store.oberth.svc", ServerName: "store.oberth.svc", CAPEM: storeTestCA, Access: runtimeconfig.SecretStoreAccess{Mount: "oberth", AuthMount: "kubernetes", WriterRole: "oberth-writer", StepRole: "oberth-step", ServerNamespace: "oberth", PipelineNamespace: "ci", ServerAccount: "oberth", Audience: "store-audience"}}
}

func secretWorkflow() *wf.Workflow {
	return &wf.Workflow{TypeMeta: meta.TypeMeta{APIVersion: "argoproj.io/v1alpha1", Kind: "Workflow"}, Spec: wf.WorkflowSpec{Entrypoint: "pipeline", Templates: []wf.Template{
		{Name: "pipeline", DAG: &wf.DAGTemplate{Tasks: []wf.DAGTask{{Name: "private", Template: "private"}, {Name: "plain", Template: "plain"}}}},
		{Name: "private", Metadata: wf.Metadata{Annotations: map[string]string{argoworkflow.SecretPathsAnnotation: "public/key"}}, Container: &core.Container{Image: "build:1", Command: []string{"oberth", "secretstore", "exec", "--", "true"}}},
		{Name: "plain", Container: &core.Container{Image: "build:1", Command: []string{"true"}}},
	}}}
}

func prepareSecretWorkflow(t *testing.T, repository string, assisted ...bool) (*admissioncontract.PreparedAdmission, error) {
	t.Helper()
	w := secretWorkflow()
	preparation := nativebinding.Preparation{Repository: "forge/org/repo", File: ".oberth/build.yaml", HelperImage: secretHelperTestImage}
	if len(assisted) > 0 && assisted[0] {
		w.Spec.Templates[1].Container.Command = []string{"true"}
		preparation.Assist, preparation.HoldTTL, preparation.HelperImage = true, time.Minute, secretHelperTestImage
	}
	document, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	sha, blob := strings.Repeat("a", 40), strings.Repeat("b", 40)
	repo := "forge/org/repo"
	file := ".oberth/build.yaml"
	r := admissioncontract.PrepareRequest{Mode: admissioncontract.ModeSubmit, Repository: repo, RunID: "run-0000000000000001", Ref: "refs/heads/topic", ObjectSHA: sha, CommitSHA: sha, Trigger: model.TriggerBranch, SelectedFile: file, FileBlob: blob, Document: document, SourceReceipt: sourcecontract.Receipt{Version: 1, Revision: sourcecontract.Revision{Repository: repo, Commit: sha}, ObjectSHA: sha, File: file, Blob: blob, DocumentDigest: sourcecontract.Digest(document)}, IdentityRequest: admissioncontract.IdentityRequest{Namespace: "ci", PipelineAccount: account(t, identityname.PipelineCI, "")}, Grants: []admissioncontract.GrantEvidence{{Repository: repository, File: file, Step: "private", Kind: "secret", Path: "public/key", Revision: "1", State: admissioncontract.StateVerified}}}
	e := argojob.New(argojob.Config{SecretStore: secretDelivery(), Accounts: fake.Accounts{Names: map[string]string{r.IdentityRequest.PipelineAccount: "pipeline-uid", account(t, identityname.StepCI, "private"): "step-uid"}}})
	return argoworkflow.Prepare(t.Context(), r, argoworkflow.Policy{}, e.Hooks(preparation))
}

func TestBareSecretExecGainsOnlyItsOwnPublicConfigAndMounts(t *testing.T) {
	t.Parallel()
	prepared, err := prepareSecretWorkflow(t, "forge/org/repo")
	if err != nil {
		t.Fatal(err)
	}
	w := prepared.View().Workflow
	private, plain := w.Spec.Templates[1], w.Spec.Templates[2]
	if len(plain.Volumes) != 0 || len(plain.Container.Env) != 0 || len(plain.Container.VolumeMounts) != 0 {
		t.Fatal("step without grant gained store access")
	}
	if len(private.Container.VolumeMounts) != 3 || len(private.Volumes) != 3 || len(private.Container.Env) != 4 {
		t.Fatal("step delivery inputs missing")
	}
	config, paths := secretEnvironment(t, private.Container.Env)
	if config != secretDelivery() || len(paths) != 1 || paths[0] != "public/key" {
		t.Fatal("step config or paths differ")
	}
	checkSecretMounts(t, private)
}

func secretEnvironment(t *testing.T, env []core.EnvVar) (runtimeconfig.SecretDelivery, []string) {
	t.Helper()
	values := map[string]string{}
	for _, v := range env {
		values[v.Name] = v.Value
	}
	var cfg runtimeconfig.SecretDelivery
	var paths []string
	if err := json.Unmarshal([]byte(values[runtimeconfig.SecretConfigEnv]), &cfg); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(values[runtimeconfig.SecretPathsEnv]), &paths); err != nil {
		t.Fatal(err)
	}
	if values[runtimeconfig.SecretTokenEnv] != runtimeconfig.SecretTokenDirectory+"/token" || values[runtimeconfig.SecretScratchEnv] != runtimeconfig.SecretScratchDirectory {
		t.Fatal("step path environment differs")
	}
	return cfg, paths
}

func TestBareSecretExecRefusesAnotherRepositoryGrant(t *testing.T) {
	t.Parallel()
	if prepared, err := prepareSecretWorkflow(t, "forge/other/repo"); err == nil || prepared != nil {
		t.Fatal("another repository grant authorized secret exec")
	}
}

func TestStepWithoutSelectionGetsNoStoreInputs(t *testing.T) {
	t.Parallel()
	w := secretWorkflow()
	err := argojob.New(argojob.Config{SecretStore: secretDelivery()}).Hooks(nativebinding.Preparation{Repository: "forge/org/repo"}).TransformNative(t.Context(), w, admissioncontract.TransformInput{})
	if err != nil {
		t.Fatal(err)
	}
	assertNoStoreInputs(t, w.Spec.Templates[1])
}

func TestGrantedStepRefusesMissingStoreConfiguration(t *testing.T) {
	t.Parallel()
	w := secretWorkflow()
	in := admissioncontract.TransformInput{SecretGrants: map[string][]admissioncontract.GrantEvidence{"private": {{Repository: "forge/org/repo", File: ".oberth/build.yaml", Step: "private", Kind: "secret", Path: "public/key"}}}}
	err := argojob.New(argojob.Config{}).Hooks(nativebinding.Preparation{Repository: "forge/org/repo", File: ".oberth/build.yaml", HelperImage: secretHelperTestImage}).TransformNative(t.Context(), w, in)
	if err == nil {
		t.Fatal("granted step silently lost store configuration")
	}
}

func TestSelectedSecretInputsAcrossContainerForms(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"container", "script", "container-set"} {
		t.Run(kind, func(t *testing.T) {
			w := secretWorkflow()
			target := &w.Spec.Templates[1]
			container := *target.Container
			container.Env = []core.EnvVar{{Name: "KEEP", Value: "authored"}, {Name: runtimeconfig.SecretPathsEnv, Value: "wrong"}}
			target.Container = nil
			switch kind {
			case "container":
				target.Container = &container
			case "script":
				target.Script = &wf.ScriptTemplate{Container: container, Source: "true"}
			case "container-set":
				target.ContainerSet = &wf.ContainerSetTemplate{Containers: []wf.ContainerNode{{Container: container}}}
			}
			grants := []admissioncontract.GrantEvidence{}
			for _, entry := range []struct{ step, path string }{{"private", "own/key"}, {"private", "own/key"}} {
				grants = append(grants, admissioncontract.GrantEvidence{Repository: "forge/org/repo", File: ".oberth/build.yaml", Step: entry.step, Kind: "secret", Path: entry.path})
			}
			e := argojob.New(argojob.Config{SecretStore: secretDelivery()})
			err := e.Hooks(nativebinding.Preparation{Repository: "forge/org/repo", File: ".oberth/build.yaml", HelperImage: secretHelperTestImage}).TransformNative(t.Context(), w, admissioncontract.TransformInput{SecretGrants: map[string][]admissioncontract.GrantEvidence{"private": grants}})
			if err != nil {
				t.Fatal(err)
			}
			env := secretTemplateEnvironment(target)
			_, paths := secretEnvironment(t, env)
			if len(paths) != 1 || paths[0] != "own/key" || len(env) != 5 || env[0].Name != "KEEP" {
				t.Fatal("step inherited another account path or duplicate environment")
			}
		})
	}
}

func checkSecretMounts(t *testing.T, private wf.Template) {
	t.Helper()
	projection := private.Volumes[0].Projected.Sources[0].ServiceAccountToken
	if projection.Audience != secretDelivery().Access.Audience || projection.Path != "token" || *projection.ExpirationSeconds != 600 {
		t.Fatal("projected store identity differs")
	}
	if private.Volumes[1].EmptyDir.Medium != core.StorageMediumMemory || !private.Container.VolumeMounts[0].ReadOnly {
		t.Fatal("private memory or read-only identity missing")
	}
	if private.Container.VolumeMounts[0].MountPath != runtimeconfig.SecretTokenDirectory || private.Container.VolumeMounts[1].MountPath != runtimeconfig.SecretScratchDirectory {
		t.Fatal("mount contract differs")
	}
}

func secretTemplateEnvironment(template *wf.Template) []core.EnvVar {
	if template.Container != nil {
		return template.Container.Env
	}
	if template.Script != nil {
		return template.Script.Env
	}
	return template.ContainerSet.Containers[0].Env
}

func TestGrantedStoreTokenPreventsAssistedHold(t *testing.T) {
	t.Parallel()
	prepared, err := prepareSecretWorkflow(t, "forge/org/repo", true)
	if err != nil {
		t.Fatal("granted step acquired a hold wrapper", err)
	}
	view := prepared.View()
	target := view.Workflow.Spec.Templates[1]
	if target.RetryStrategy != nil || len(target.InitContainers) != 1 || target.InitContainers[0].Name != "oberth-secret-install" || target.PodSpecPatch != "" {
		t.Fatal("granted step acquired a hold wrapper")
	}
	for _, entry := range view.Plan.Entries {
		if entry.Template == "private" && (!entry.CredentialIntent || entry.HoldEligible || entry.HoldArmed) {
			t.Fatal("granted step acquired a hold wrapper")
		}
	}
}

const storeTestCA = `-----BEGIN CERTIFICATE-----
MIIBSjCB/aADAgECAhRw6aqavONHQ8Gg1w4xXmsF9jtCFjAFBgMrZXAwGzEZMBcG
A1UEAwwQc3RvcmUub2JlcnRoLnN2YzAeFw0yNjExMTgwMTE5NTdaFw0yNjExMTkw
MTE5NTdaMBsxGTAXBgNVBAMMEHN0b3JlLm9iZXJ0aC5zdmMwKjAFBgMrZXADIQBr
9FDoe0FsSw+pmHagfkuvMtdJmINbF2/dAb+BL9iEl6NTMFEwHQYDVR0OBBYEFP+E
+xQtPlj6JHT15SKXerkuWj4UMB8GA1UdIwQYMBaAFP+E+xQtPlj6JHT15SKXerku
Wj4UMA8GA1UdEwEB/wQFMAMBAf8wBQYDK2VwA0EAVGI5va7YFn8ujckXmZ13nZyu
QZ1cPevD1c/3pVHQU0Fwx0livbzU6U/Axmg8TqrApmiMD7VyydVEzOcK0WmBDA==
-----END CERTIFICATE-----
`
