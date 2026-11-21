package argoworkflow

import (
	"context"
	"reflect"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	corev1 "k8s.io/api/core/v1"
)

// twoSecretSteps is one file whose steps read different store paths.
func twoSecretSteps() *wfv1.Workflow {
	step := func(name, path string) wfv1.Template {
		t := wfv1.Template{Name: name, Container: &corev1.Container{Image: "example.test/image:1", Command: []string{"oberth", "secretstore", "exec", "--", "true"}}}
		declareSecretPaths(&t, path)
		return t
	}
	w := simpleWorkflow()
	w.Spec.Templates = []wfv1.Template{
		{Name: "main", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "sign", Template: "sign"}, {Name: "push", Template: "push", Depends: "sign"}}}},
		step("sign", "team/signing"),
		step("push", "team/registry"),
	}
	return w
}

func prepareSecretSteps(t *testing.T, w *wfv1.Workflow, grants ...admissioncontract.GrantEvidence) (admissioncontract.TransformInput, error) {
	t.Helper()
	r := requestFor(t, w)
	r.Grants = grants
	hooks := identityHooks()
	hooks.LoginAudience = "login"
	var input admissioncontract.TransformInput
	hooks.TransformNative = func(_ context.Context, _ *wfv1.Workflow, in admissioncontract.TransformInput) error {
		input = in
		return nil
	}
	_, err := Prepare(t.Context(), r, Policy{}, hooks)
	return input, err
}

func TestStepReceivesExactlyItsDeclaredPaths(t *testing.T) {
	t.Parallel()
	sign, push := clusterGrant("sign", "secret", "team/signing"), clusterGrant("push", "secret", "team/registry")
	input, err := prepareSecretSteps(t, twoSecretSteps(), sign, push, clusterGrant("*", "secret", "team/registry"))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]admissioncontract.GrantEvidence{"sign": {sign}, "push": {push}}
	if !reflect.DeepEqual(input.SecretGrants, want) {
		t.Fatalf("secret delivery differs from the declarations: %+v", input.SecretGrants)
	}
	if input.CredentialSteps["sign"] != "sign" || input.CredentialSteps["push"] != "push" {
		t.Fatal("store accounts differ from the declaring steps", input.CredentialSteps)
	}
}

// Admission hands each step only its own declared paths. Under `*` grants both
// steps still share the one wildcard store account, which can reach every `*`
// path of the file; only grants by step name give each step its own account.
func TestAdmissionPathListFollowsEachStepsDeclaration(t *testing.T) {
	t.Parallel()
	signing, registry := clusterGrant("*", "secret", "team/signing"), clusterGrant("*", "secret", "team/registry")
	input, err := prepareSecretSteps(t, twoSecretSteps(), signing, registry)
	want := map[string][]admissioncontract.GrantEvidence{"sign": {signing}, "push": {registry}}
	if err != nil || !reflect.DeepEqual(input.SecretGrants, want) {
		t.Fatal("admission path lists differ from the declarations", err, input.SecretGrants)
	}
	if input.CredentialSteps["sign"] != "*" || input.CredentialSteps["push"] != "*" {
		t.Fatal("wildcard grants did not select the one wildcard account", input.CredentialSteps)
	}
	grants := []admissioncontract.GrantEvidence{clusterGrant("sign", "secret", "team/signing"), clusterGrant("push", "secret", "team/registry")}
	crossed := append(grants, clusterGrant("push", "secret", "team/signing"))
	input, err = prepareSecretSteps(t, twoSecretSteps(), crossed...)
	if err != nil || !reflect.DeepEqual(input.SecretGrants["push"], []admissioncontract.GrantEvidence{grants[1]}) {
		t.Fatal("a named grant the step does not declare was delivered or refused", err, input.SecretGrants)
	}
	if input.CredentialSteps["sign"] != "sign" || input.CredentialSteps["push"] != "push" {
		t.Fatal("named grants did not select one account per step", input.CredentialSteps)
	}
}

// One grant set serves every commit: a step receives what it declares and is
// granted, an unused grant is not delivered, and a declared path needs a grant.
func TestStepReceivesItsDeclarationAndGrantsIntersection(t *testing.T) {
	t.Parallel()
	a, b := clusterGrant("build", "secret", "team/a"), clusterGrant("build", "secret", "team/b")
	for _, tc := range []struct {
		declared string
		want     []admissioncontract.GrantEvidence
	}{{"team/a", []admissioncontract.GrantEvidence{a}}, {"", nil}} {
		input, err := prepareSecretSteps(t, buildStep(tc.declared), a, b)
		if err != nil || !reflect.DeepEqual(input.SecretGrants["build"], tc.want) {
			t.Fatal("delivery differs from the declared and granted paths", tc.declared, err, input.SecretGrants)
		}
	}
	_, err := prepareSecretSteps(t, buildStep("team/c"), a, b)
	findings := admissionFindings(t, err)
	const command = "oberth secret put --repo 'forge/org/repo' --file '.oberth/build.yaml' --step 'build' 'team/c' < <value-file>"
	if len(findings) != 1 || findings[0].Code != "grant" || findings[0].Command != command {
		t.Fatal("a declared path without a grant was not refused with its command", findings)
	}
}

// buildStep is one container step named build; it calls the helper only when it declares paths.
func buildStep(declared string) *wfv1.Workflow {
	w := simpleWorkflow()
	w.Spec.Entrypoint, w.Spec.Templates[0].Name = "build", "build"
	if declared != "" {
		w.Spec.Templates[0].Container.Command = []string{"oberth", "secretstore", "exec", "--", "true"}
		declareSecretPaths(&w.Spec.Templates[0], declared)
	}
	return w
}

func TestWorkflowWideSecretPathsAreRefused(t *testing.T) {
	t.Parallel()
	w := twoSecretSteps()
	w.Annotations = map[string]string{SecretPathsAnnotation: "team/signing,team/registry"}
	_, err := prepareSecretSteps(t, w, clusterGrant("sign", "secret", "team/signing"), clusterGrant("push", "secret", "team/registry"))
	findings := admissionFindings(t, err)
	if len(findings) != 1 || findings[0].Code != "workflow_secret_paths" || findings[0].Path != "metadata.annotations["+SecretPathsAnnotation+"]" {
		t.Fatal("workflow-wide declaration admitted", findings)
	}
}

func TestDeclaredPathWithoutGrantPrintsItsStepCommand(t *testing.T) {
	t.Parallel()
	_, err := prepareSecretSteps(t, twoSecretSteps(), clusterGrant("sign", "secret", "team/signing"))
	findings := admissionFindings(t, err)
	const command = "oberth secret put --repo 'forge/org/repo' --file '.oberth/build.yaml' --step 'push' 'team/registry' < <value-file>"
	if len(findings) != 1 || findings[0].Code != "grant" || findings[0].Path != `spec.templates["push"].secrets[team/registry]` || findings[0].Command != command {
		t.Fatal("missing grant does not name file, step and path", findings)
	}
}

func TestTemplateDefaultsCannotDeclareSecretPaths(t *testing.T) {
	t.Parallel()
	w := simpleWorkflow()
	w.Spec.TemplateDefaults = &wfv1.Template{}
	declareSecretPaths(w.Spec.TemplateDefaults, "team/key")
	_, err := prepareSecretSteps(t, w, clusterGrant("main", "secret", "team/key"))
	requireCode(t, err, "reserved")
}

// A DAG parent, a suspend gate and a resource step start no container that
// could read a secret: the declaration is refused and no grant is asked for.
func TestDeclarationOnTemplateWithoutContainerIsRefused(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"dag", "suspend", "resource"} {
		w, policy := twoSecretSteps(), Policy{}
		target := &w.Spec.Templates[0]
		switch kind {
		case "suspend":
			w.Spec.Templates[2] = wfv1.Template{Name: "push", Suspend: &wfv1.SuspendTemplate{}}
			target = &w.Spec.Templates[2]
		case "resource":
			var fixture admissionFixture
			w, fixture = resourceWorkflow("apiVersion: example.test/v1\nkind: Widget\nmetadata: {name: test, namespace: resources}")
			policy, target = fixture.Policy, &w.Spec.Templates[0]
		}
		declareSecretPaths(target, "team/unread")
		r := requestFor(t, w)
		r.IdentityRequest.ResourceAccount = "resources"
		r.Grants = []admissioncontract.GrantEvidence{clusterGrant("sign", "secret", "team/signing"), clusterGrant("push", "secret", "team/registry")}
		hooks := identityHooks()
		hooks.LoginAudience = "login"
		_, err := Prepare(t.Context(), r, policy, hooks)
		findings := admissionFindings(t, err)
		want := `spec.templates["` + target.Name + `"].metadata.annotations[` + SecretPathsAnnotation + "]"
		if len(findings) != 1 || findings[0].Code != "secret_form" || findings[0].Path != want || findings[0].Command != "" {
			t.Fatal("declaration on a template without a container was not refused alone", kind, findings)
		}
	}
}

// A malformed or repeated path is one path fault naming the step, never a
// request for a grant the operator could not give.
func TestMalformedSecretPathIsOnePathFault(t *testing.T) {
	t.Parallel()
	for declared, bad := range map[string]string{"team/a/": "team/a/", "team/a,": "", "team/../other/a": "team/../other/a", "team/a,team/a": "team/a", "team/*": "team/*"} {
		_, err := prepareSecretSteps(t, buildStep(declared), clusterGrant("build", "secret", "team/a"))
		findings := admissionFindings(t, err)
		want := `spec.templates["build"].metadata.annotations[` + SecretPathsAnnotation + "][" + bad + "]"
		if len(findings) != 1 || findings[0].Code != "secret_path" || findings[0].Path != want || findings[0].Command != "" {
			t.Fatal("malformed path is not one path fault", declared, findings)
		}
	}
}
