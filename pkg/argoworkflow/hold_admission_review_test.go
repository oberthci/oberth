package argoworkflow

import (
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"

	"github.com/oberthci/oberth/internal/admissioncontract"
)

func reviewTwoStep(b wfv1.Template) *wfv1.Workflow {
	w := simpleWorkflow()
	w.Spec.Volumes = []corev1.Volume{{Name: "s", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "private"}}}}
	a := wfv1.Template{Name: "a", Container: &corev1.Container{Image: "example.test/image:1", Command: []string{"true"}, VolumeMounts: []corev1.VolumeMount{{Name: "s", MountPath: "/s"}}}}
	w.Spec.Templates = []wfv1.Template{{Name: "main", Steps: []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{Name: "a", Template: "a"}}}, {Steps: []wfv1.WorkflowStep{{Name: "b", Template: "b"}}}}}, a, b}
	return w
}

// A workflow-level secret mounted by granted step a and, through
// containerSet.volumeMounts (what Argo uses to add the volume), by step b.
func TestReviewContainerSetSharedSecretNeedsGrant(t *testing.T) {
	t.Parallel()
	b := wfv1.Template{Name: "b", ContainerSet: &wfv1.ContainerSetTemplate{VolumeMounts: []corev1.VolumeMount{{Name: "s", MountPath: "/s"}}, Containers: []wfv1.ContainerNode{{Container: corev1.Container{Name: "main", Image: "example.test/image:1", Command: []string{"true"}}}}}}
	w := reviewTwoStep(b)
	r := requestFor(t, w)
	r.Grants = []admissioncontract.GrantEvidence{clusterGrant("a", "cluster-secret", "private")}
	if _, err := Prepare(t.Context(), r, Policy{}, identityHooks()); err == nil {
		t.Fatal("step b reads workflow secret s with no grant for b")
	} else {
		t.Log("refused:", err)
	}
}

// Control: b mounts it in its own container; must be refused without b's grant.
func TestReviewContainerSharedSecretNeedsGrant(t *testing.T) {
	t.Parallel()
	b := wfv1.Template{Name: "b", Container: &corev1.Container{Image: "example.test/image:1", Command: []string{"true"}, VolumeMounts: []corev1.VolumeMount{{Name: "s", MountPath: "/s"}}}}
	w := reviewTwoStep(b)
	r := requestFor(t, w)
	r.Grants = []admissioncontract.GrantEvidence{clusterGrant("a", "cluster-secret", "private")}
	if _, err := Prepare(t.Context(), r, Policy{}, identityHooks()); err == nil {
		t.Fatal("container b admitted without grant")
	}
}
