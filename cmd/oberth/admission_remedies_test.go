package main

import (
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestAdmissionRemedyCommandsExistAndExecuteInTable(t *testing.T) {
	for _, kind := range []string{"secret", "identity", "secret_key", "configmap_key", "pull_secret", "pvc", "pvc-release", "device"} {
		t.Run(kind, func(t *testing.T) { checkAdmissionRemedies(t, kind) })
	}
}
func checkAdmissionRemedies(t *testing.T, kind string) {
	t.Helper()
	findings := preparedRemedies(t, kind)
	count := 0
	for _, finding := range findings {
		if finding.Command == "" || finding.Code == "live" {
			continue
		}
		count++
		words := remedyWords(t, strings.TrimSuffix(finding.Command, " < <value-file>"))
		row, _, ok := findCommand(commands(), words[1:])
		if kind != "secret" && (!ok || row.definition.Name != "access_allow") {
			t.Fatalf("remedy missing from real table: %s", finding.Command)
		}
		bindings := fake.Bindings()
		handler := &fake.Handler[apicontract.AccessAllowArgs, apicontract.Grant]{}
		bindings.AccessAllow.Handler = handler
		bindings.SecretPut.Handler = &fake.Handler[apicontract.SecretPutArgs, apicontract.Receipt]{Value: apicontract.Receipt{ID: "team/signing-key", State: "stored"}}
		app, _, diagnostic := fixture(t, bindings)
		app.in = strings.NewReader("fixture-value")
		if code := app.run(t.Context(), words[1:]); code != 0 {
			t.Fatalf("printed remedy refused (%d): %s / %s", code, finding.Command, diagnostic.String())
		}
		inputs, _, calls := handler.Observed()
		if calls != 1 || inputs[0].Repo != "forge/org/repo" {
			t.Fatal("recipe did not reach canonical binding")
		}
		assertGrantRecipe(t, inputs[0], kind)
		assertServerGrantKind(t, inputs[0])
	}
	if count == 0 {
		t.Fatal("fixture emitted no remedy command")
	}
}
func preparedRemedies(t *testing.T, kind string) []admissioncontract.Finding {
	t.Helper()
	document, policy := remedyWorkflow(kind)
	document.APIVersion, document.Kind = "argoproj.io/v1alpha1", "Workflow"
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	r := admissioncontract.PrepareRequest{Mode: admissioncontract.ModeInspect, Repository: "forge/org/repo", SelectedFile: ".oberth/build.yaml", Document: raw, Trigger: "branch"}
	if policy.Release {
		r.Trigger = "tag"
	}
	prepared, err := argoworkflow.Prepare(t.Context(), r, policy, admissioncontract.PrepareHooks{LoginAudience: "secretstore"})
	if err == nil {
		return prepared.View().Findings
	}
	var refused *argoworkflow.AdmissionError
	if !errors.As(err, &refused) {
		t.Fatal(err)
	}
	return append(refused.Findings, refused.Needs...)
}

func remedyWords(t *testing.T, command string) []string {
	t.Helper()
	shell := exec.CommandContext(t.Context(), "sh", "-c", `set -- `+command+`; printf '%s\000' "$@"`)
	output, err := shell.Output()
	if err != nil {
		t.Fatalf("printed command cannot be parsed by a shell: %v", err)
	}
	return strings.Split(strings.TrimSuffix(string(output), "\x00"), "\x00")
}

// assertServerGrantKind checks a printed recipe against the server's own grant
// table, which the fake handler does not consult.
func assertServerGrantKind(t *testing.T, args apicontract.AccessAllowArgs) {
	t.Helper()
	values := map[string]string{"repo": args.Repo, "workflow": args.Workflow, "step": args.Step, "secret": args.Secret, "audience": args.Audience, "claim": args.Claim, "tier": args.Tier}
	_, known := apicontract.GrantArguments(args.Kind)
	if name, _ := apicontract.GrantArgumentProblem(args.Kind, func(n string) bool { return values[n] != "" }); !known || name != "" {
		t.Fatalf("server refuses printed grant kind %q (%s): %+v", args.Kind, name, args)
	}
}
func assertGrantRecipe(t *testing.T, args apicontract.AccessAllowArgs, kind string) {
	t.Helper()
	if strings.HasPrefix(kind, "pvc") {
		want := "ci"
		if kind == "pvc-release" {
			want = "release"
		}
		if args.Claim != "data" || args.Tier != want || args.Workflow != "" {
			t.Fatalf("wrong PVC recipe: %+v", args)
		}
		return
	}
	if args.Workflow != ".oberth/build.yaml" || args.Step != "main" || args.Secret == "" {
		t.Fatalf("wrong step recipe: %+v", args)
	}
	if args.Kind == "identity" && args.Audience != "secretstore" {
		t.Fatal("identity audience lost")
	}
}
func remedyWorkflow(kind string) (*wf.Workflow, argoworkflow.Policy) {
	document := &wf.Workflow{ObjectMeta: meta.ObjectMeta{}, Spec: wf.WorkflowSpec{Entrypoint: "main", Templates: []wf.Template{{Name: "main", Container: &core.Container{Image: "example.test/build:1", Command: []string{"true"}}}}}}
	template := &document.Spec.Templates[0]
	policy := argoworkflow.Policy{}
	switch kind {
	case "secret":
		template.Metadata.Annotations = map[string]string{argoworkflow.SecretPathsAnnotation: "team/signing-key"}
		template.Container.Command = []string{"oberth", "secretstore", "exec", "--", "true"}
	case "identity":
		template.Volumes = []core.Volume{{Name: "login", VolumeSource: core.VolumeSource{Projected: &core.ProjectedVolumeSource{Sources: []core.VolumeProjection{{ServiceAccountToken: &core.ServiceAccountTokenProjection{Audience: "secretstore", Path: "token"}}}}}}}
		template.Container.VolumeMounts = []core.VolumeMount{{Name: "login", MountPath: "/var/run/secrets/secretstore", ReadOnly: true}}
	case "secret_key":
		template.Container.Env = []core.EnvVar{{Name: "VALUE", ValueFrom: &core.EnvVarSource{SecretKeyRef: &core.SecretKeySelector{LocalObjectReference: core.LocalObjectReference{Name: "deploy"}, Key: "value"}}}}
	case "configmap_key":
		template.Container.Env = []core.EnvVar{{Name: "VALUE", ValueFrom: &core.EnvVarSource{ConfigMapKeyRef: &core.ConfigMapKeySelector{LocalObjectReference: core.LocalObjectReference{Name: "deploy"}, Key: "value"}}}}
	case "pull_secret":
		document.Spec.ImagePullSecrets = []core.LocalObjectReference{{Name: "registry"}}
	case "device":
		template.Container.Resources.Limits = core.ResourceList{"devices.kubevirt.io/kvm": resource.MustParse("1")}
	case "pvc", "pvc-release":
		policy.Release = kind == "pvc-release"
		template.Volumes = []core.Volume{{Name: "data", VolumeSource: core.VolumeSource{PersistentVolumeClaim: &core.PersistentVolumeClaimVolumeSource{ClaimName: "data"}}}}
		template.Container.VolumeMounts = []core.VolumeMount{{Name: "data", MountPath: "/data"}}
	}
	return document, policy
}
