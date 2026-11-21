package argoworkflow

import (
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	corev1 "k8s.io/api/core/v1"
)

func TestSharedSecretRefusalExplainsWhyConsumerGrantCannotApply(t *testing.T) {
	t.Parallel()
	shared := sharedObject("shared")
	shared.Spec.Templates[0].Container.Env = []corev1.EnvVar{{Name: "PRIVATE", ValueFrom: &corev1.EnvVarSource{
		SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "private"}, Key: "token"},
	}}}
	policy := Policy{TemplateGetter: getterFor(map[string]*wfv1.WorkflowTemplate{"shared": shared})}
	r := requestFor(t, sharedConsumer("shared", "work"))
	r.Grants = []admissioncontract.GrantEvidence{clusterGrant("*", "cluster-secret", "private")}
	prepared, err := Prepare(t.Context(), r, policy, identityHooks())
	const want = `.oberth/build.yaml: templates[shared].templates["work"].container.env[PRIVATE].valueFrom.secretKeyRef: shared templates inherit no consumer grants; Remove the selector or put this step in the repository file before granting it.`
	if prepared != nil || err == nil || err.Error() != want {
		t.Fatalf("shared selector: sealed=%v, error=%v; want %s", prepared != nil, err, want)
	}
	local := simpleWorkflow()
	local.Spec.Templates[0].Container = shared.Spec.Templates[0].Container.DeepCopy()
	request := requestFor(t, local)
	request.Grants = r.Grants
	if _, err := Prepare(t.Context(), request, policy, identityHooks()); err != nil {
		t.Fatal("moving the step into the repository did not make its grant apply", err)
	}
}
