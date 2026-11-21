package argoworkflow

import (
	"context"
	"reflect"
	"testing"

	"github.com/oberthci/oberth/internal/admissioncontract"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestAuthoredFinalizersRefuseBeforePreparation(t *testing.T) {
	t.Parallel()
	for _, where := range []string{"metadata.finalizers", "spec.volumeClaimTemplates[cache].metadata.finalizers"} {
		t.Run(where, func(t *testing.T) {
			w := simpleWorkflow()
			if where == "metadata.finalizers" {
				w.Finalizers = []string{"example.test/never"}
			} else {
				w.Spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{{ObjectMeta: metav1.ObjectMeta{Name: "cache", Finalizers: []string{"example.test/never"}}}}
			}
			before := w.DeepCopy()
			hooks := admissioncontract.PrepareHooks{VerifyIdentities: func(_ context.Context, _ admissioncontract.IdentityRequest) (admissioncontract.IdentityEvidence, error) {
				t.Fatal("refusal reached account verification")
				return admissioncontract.IdentityEvidence{}, nil
			}}
			result, err := Prepare(t.Context(), requestFor(t, w), Policy{}, hooks)
			if result != nil || !reflect.DeepEqual(w, before) {
				t.Fatal("refusal changed authored workflow")
			}
			findings := admissionFindings(t, err)
			if len(findings) != 1 || findings[0].Path != where || findings[0].Code != "reserved" {
				t.Fatal(findings)
			}
		})
	}
}
