package argoworkflow

import (
	"testing"

	"github.com/oberthci/oberth/internal/admissioncontract"
	corev1 "k8s.io/api/core/v1"
)

func TestPublicTrustConfigMapNeedsTheSameGrantAsOtherData(t *testing.T) {
	t.Parallel()
	w := simpleWorkflow()
	w.Spec.Templates[0].Volumes = []corev1.Volume{{Name: "ca", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "public-ca"}}}}}
	r := requestFor(t, w)
	result, err := Prepare(t.Context(), r, Policy{}, identityHooks())
	requireCode(t, err, "read")
	if result != nil {
		t.Fatal("ungranted ConfigMap sealed")
	}
	r.Grants = []admissioncontract.GrantEvidence{clusterGrant("main", "configmap", "public-ca")}
	if _, err = Prepare(t.Context(), r, Policy{}, identityHooks()); err != nil {
		t.Fatal("granted ConfigMap refused", err)
	}
}
