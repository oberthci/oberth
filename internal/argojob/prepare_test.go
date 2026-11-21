package argojob_test

import (
	"context"
	"encoding/json"
	"github.com/oberthci/oberth/internal/nativebinding"
	"k8s.io/apimachinery/pkg/util/intstr"
	"strings"
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	argoutil "github.com/argoproj/argo-workflows/v4/workflow/util"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/argojob/fake"
	"github.com/oberthci/oberth/internal/identityname"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func fixture(t *testing.T, options ...nativebinding.Preparation) (*admissioncontract.PreparedAdmission, *argojob.Executor, *fake.Workflows) {
	t.Helper()
	return fixtureKind(t, "container", options...)
}
func fixtureKind(t *testing.T, kind string, options ...nativebinding.Preparation) (*admissioncontract.PreparedAdmission, *argojob.Executor, *fake.Workflows) {
	t.Helper()
	w := fixtureWorkflow(kind)
	policy := definitionFormInput(w, kind)
	document, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	sha, blob := strings.Repeat("a", 40), strings.Repeat("b", 40)
	r := admissioncontract.PrepareRequest{Mode: admissioncontract.ModeSubmit, Repository: "forge/org/repo", RunID: "run-0000000000000001", Ref: "refs/heads/topic", ObjectSHA: sha, CommitSHA: sha, Trigger: model.TriggerBranch, SelectedFile: ".oberth/build.yaml", FileBlob: blob, Document: document,
		SourceReceipt:   sourcecontract.Receipt{Version: 1, Revision: sourcecontract.Revision{Repository: "forge/org/repo", Commit: sha}, ObjectSHA: sha, File: ".oberth/build.yaml", Blob: blob, DocumentDigest: sourcecontract.Digest(document)},
		IdentityRequest: admissioncontract.IdentityRequest{Namespace: "ci", PipelineAccount: account(t, identityname.PipelineCI, "")}}
	if policy.Capability != nil {
		r.IdentityRequest.ResourceAccount = policy.Capability.ServiceAccount
	}
	f := &fake.Workflows{}
	e := argojob.New(argojob.Config{Workflows: f, Accounts: fake.Accounts{Names: map[string]string{r.IdentityRequest.PipelineAccount: "pipeline-uid", "resources": "resource-uid"}}})
	preparation := nativebinding.Preparation{}
	if len(options) > 0 {
		preparation = options[0]
	}
	preparation.Repository, preparation.File = r.Repository, r.SelectedFile
	hooks := e.Hooks(preparation)
	if kind == "definition-probe" {
		hooks.BindSource = func(_ context.Context, d sourcecontract.Descriptor) (sourcecontract.Binding, error) {
			return sourcecontract.Binding{Descriptor: d, RepositoryID: 1, Revision: r.SourceReceipt.Revision, Export: sourcecontract.Export{Ref: sourcecontract.Ref{Name: r.Ref, ObjectSHA: sha, CommitSHA: sha}, DefaultBranch: sourcecontract.Ref{Name: "refs/heads/main", ObjectSHA: sha, CommitSHA: sha}}}, nil
		}
	}
	p, err := argoworkflow.Prepare(t.Context(), r, policy, hooks)
	if err != nil {
		t.Fatal(err)
	}
	runs := memoryAdmission(t, p)
	e = argojob.New(argojob.Config{InstanceID: "frontdoor", Workflows: f, Runs: runs, Progress: runs})
	return p, e, f
}
func fixtureWorkflow(kind string) *wf.Workflow {
	w := &wf.Workflow{ObjectMeta: meta.ObjectMeta{Labels: map[string]string{"pipeline": "authored"}, Annotations: map[string]string{"note": "authored"}}, TypeMeta: meta.TypeMeta{APIVersion: "argoproj.io/v1alpha1", Kind: "Workflow"}, Spec: wf.WorkflowSpec{Entrypoint: "build", Templates: []wf.Template{{Name: "build", Container: &core.Container{Image: "example.test/build:1", Command: []string{"true"}}}}}}
	if kind == "script" || strings.HasSuffix(kind, "-script") {
		w.Spec.Templates[0].Script = &wf.ScriptTemplate{Container: *w.Spec.Templates[0].Container, Source: "true"}
		w.Spec.Templates[0].Container = nil
	}
	if strings.HasPrefix(kind, "review-inline-") {
		inlineFixtureShape(w, kind)
	}
	if strings.HasPrefix(kind, "review-defaults") {
		w.Spec.TemplateDefaults = &wf.Template{Container: &core.Container{Command: []string{"sh"}, Env: []core.EnvVar{{Name: "FROM_DEFAULTS", Value: "1"}}}}
		w.Spec.Templates[0].Container.Command = nil
		if strings.HasSuffix(kind, "ineligible") {
			w.Spec.Templates[0].Container.ReadinessProbe = &core.Probe{ProbeHandler: core.ProbeHandler{Exec: &core.ExecAction{Command: []string{"true"}}}}
		}
	}
	if kind == "review-inherited-retry" {
		w.Spec.RetryStrategy = &wf.RetryStrategy{Limit: new(intstr.FromInt(2))}
	}
	if kind == "definition-probe" {
		definitionProbeInput(w)
	}
	return w
}

func account(t *testing.T, kind identityname.Kind, step string) string {
	t.Helper()
	k := identityname.Key{Repository: "forge/org/repo"}
	if step != "" {
		k.File = ".oberth/build.yaml"
		k.Step = step
	}
	name, err := identityname.Name(k, kind)
	if err != nil {
		t.Fatal(err)
	}
	return name
}

// Covers: REQ-C5-002
func TestPrivateExecutorAuthorityContract(t *testing.T) {
	t.Parallel()
	p, _, _ := fixture(t)
	w := p.View().Workflow
	mount := core.VolumeMount{Name: "exec-sa-token", MountPath: "/var/run/secrets/kubernetes.io/serviceaccount", ReadOnly: true}
	pod := core.PodSpec{AutomountServiceAccountToken: w.Spec.AutomountServiceAccountToken, Volumes: []core.Volume{{Name: "exec-sa-token", VolumeSource: core.VolumeSource{Secret: &core.SecretVolumeSource{SecretName: "native-token"}}}}, Containers: []core.Container{{Name: "main"}, {Name: "wait", VolumeMounts: []core.VolumeMount{mount}}}, InitContainers: []core.Container{{Name: "init", VolumeMounts: []core.VolumeMount{mount}}, {Name: "init-artifact-oberth", VolumeMounts: []core.VolumeMount{mount}}}}
	got, err := argoutil.ApplyPodSpecPatch(pod, w.Spec.PodSpecPatch)
	if err != nil {
		t.Fatal(err)
	}
	checkExecutorTokenProjection(t, got.Volumes[0])
	if *got.AutomountServiceAccountToken || len(got.Containers[0].VolumeMounts) != 0 || len(got.Containers[1].VolumeMounts) != 1 || len(got.InitContainers[1].VolumeMounts) != 1 {
		t.Fatal("native mounts changed")
	}
	if w.Spec.Executor.ServiceAccountName != w.Spec.Templates[0].ServiceAccountName {
		t.Fatal("executor lookup must use pipeline")
	}
}

func checkExecutorTokenProjection(t *testing.T, volume core.Volume) {
	t.Helper()
	if volume.Secret != nil || volume.Projected == nil || len(volume.Projected.Sources) != 3 {
		t.Fatalf("token source: %+v", volume)
	}
	if volume.Projected.Sources[0].ServiceAccountToken == nil || volume.Projected.Sources[0].ServiceAccountToken.ExpirationSeconds == nil || *volume.Projected.Sources[0].ServiceAccountToken.ExpirationSeconds != 3600 {
		t.Fatal("token expiry")
	}
	token := volume.Projected.Sources[0].ServiceAccountToken
	if token.Path != "token" || token.Audience != "" {
		t.Fatal("source plugin reads the native mount's token with the API server audience")
	}
}

// Covers: REQ-C5-007
func TestSeparateCiAndReleaseIdentityContract(t *testing.T) {
	t.Parallel()
	for _, release := range []bool{false, true} {
		for _, step := range []string{"", "*", "build"} {
			t.Run(step+string(rune('0'+boolInt(release))), func(t *testing.T) { checkAccountSelection(t, release, step) })
		}
	}
}
func checkAccountSelection(t *testing.T, release bool, step string) {
	t.Helper()
	kind := selectionKind(release, step != "")
	want := account(t, kind, step)
	e := argojob.New(argojob.Config{SecretStore: secretDelivery(), Accounts: fake.Accounts{Names: map[string]string{want: "uid"}}})
	w := &wf.Workflow{Spec: wf.WorkflowSpec{Templates: []wf.Template{{Name: "build", Container: &core.Container{}}}}}
	in := admissioncontract.TransformInput{Mode: admissioncontract.ModeSubmit, Identity: admissioncontract.IdentityEvidence{Pipeline: admissioncontract.AccountEvidence{Name: "pipeline"}}}
	if step != "" {
		in.SecretGrants = map[string][]admissioncontract.GrantEvidence{"build": {{Repository: "forge/org/repo", File: ".oberth/build.yaml", Step: step, Kind: "secret"}}}
		in.CredentialSteps = map[string]string{"build": step}
	}
	p := nativebinding.Preparation{Repository: "forge/org/repo", File: ".oberth/build.yaml", Release: release, HelperImage: secretHelperTestImage}
	if err := e.Hooks(p).TransformNative(t.Context(), w, in); err != nil || w.Spec.Templates[0].ServiceAccountName != want {
		t.Fatalf("account %s: %v", w.Spec.Templates[0].ServiceAccountName, err)
	}
	e = argojob.New(argojob.Config{Accounts: fake.Accounts{Names: map[string]string{}}})
	if err := e.Hooks(p).TransformNative(t.Context(), w, in); err == nil {
		t.Fatal("missing account accepted")
	}
}
func selectionKind(release, step bool) identityname.Kind {
	if step {
		if release {
			return identityname.Step
		}
		return identityname.StepCI
	}
	if release {
		return identityname.Pipeline
	}
	return identityname.PipelineCI
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
func TestSafeDirectoryOnce(t *testing.T) {
	t.Parallel()
	e := argojob.New(argojob.Config{})
	for _, kind := range []sourcecontract.Kind{sourcecontract.Main, sourcecontract.Pinned} {
		w := &wf.Workflow{Spec: wf.WorkflowSpec{Templates: []wf.Template{{Name: "build", Container: &core.Container{Env: []core.EnvVar{{Name: "GIT_CONFIG_COUNT", Value: "1"}, {Name: "GIT_CONFIG_KEY_0", Value: "author.name"}, {Name: "GIT_CONFIG_VALUE_0", Value: "author"}}}}}}}
		in := admissioncontract.TransformInput{SourceBindings: []sourcecontract.Binding{{Template: "build", Descriptor: sourcecontract.Descriptor{Kind: kind}}}}
		hook := e.Hooks(nativebinding.Preparation{Repository: "forge/org/repo"})
		for range 2 {
			if err := hook.TransformNative(context.Background(), w, in); err != nil {
				t.Fatal(err)
			}
		}
		env := w.Spec.Templates[0].Container.Env
		want := 3
		if kind == sourcecontract.Main {
			want = 5
		}
		if len(env) != want || env[1].Value != "author.name" {
			t.Fatalf("env: %+v", env)
		}
	}
}
