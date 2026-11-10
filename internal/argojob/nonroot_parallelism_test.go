package argojob

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/pipeline"
)

// withProfile points the verifier fixture at another canonical profile.
func withProfile(pod *corev1.Pod, cm *corev1.ConfigMap, profile argoworkflow.NonrootProfile) {
	cm.Name, cm.Data = profile.ConfigMap, map[string]string{"config": profile.Config}
	pod.Spec.Containers[0].Args[1] = profile.ConfigMap
	pod.Spec.Volumes[0].ConfigMap.Name = profile.ConfigMap
}

// A profile carrying controller parallelism (#1517) verifies under its own
// pinned name, and its digest enters the submission binding; the pin must
// match the running controller, and a hand-edited config under the pinned
// name is refused.
func TestNonrootVerifierAcceptsTheParallelismProfile(t *testing.T) {
	profile, err := argoworkflow.NewNonrootProfile(6, 0)
	if err != nil {
		t.Fatal(err)
	}
	pod, cm := nonrootVerifierObjects()
	withProfile(pod, cm, profile)
	proof, err := verifyNonrootController(t.Context(), fake.NewClientset(pod, cm), testNamespace, profile.ConfigMap, testRequest(pipeline.TriggerCI, nonrootTestDocument))
	if err != nil || proof.configSHA256 != profile.SHA256 {
		t.Fatalf("parallelism profile = %+v, %v", proof, err)
	}
	base, baseCM := nonrootVerifierObjects()
	baseProof, err := verifyNonrootController(t.Context(), fake.NewClientset(base, baseCM), testNamespace, "", testRequest(pipeline.TriggerCI, nonrootTestDocument))
	if err != nil || baseProof.binding() == proof.binding() {
		t.Fatal("the profile digest does not bind the submission")
	}
	if _, err := verifyNonrootController(t.Context(), fake.NewClientset(pod, cm), testNamespace, argoworkflow.NonrootProfileConfigMap, testRequest(pipeline.TriggerCI, nonrootTestDocument)); err == nil {
		t.Fatal("a controller on another profile passed an explicit built-in pin")
	}
	edited, editedCM := nonrootVerifierObjects()
	withProfile(edited, editedCM, profile)
	editedCM.Data["config"] = argoworkflow.NonrootProfileConfig
	for _, pin := range []string{profile.ConfigMap, ""} {
		if _, err := verifyNonrootController(t.Context(), fake.NewClientset(edited, editedCM), testNamespace, pin, testRequest(pipeline.TriggerCI, nonrootTestDocument)); err == nil {
			t.Fatalf("pin %q: non-canonical content under a profile name accepted", pin)
		}
	}
}

// Without a pin the server follows the profile the controller runs, so an
// Argo limits change needs no Oberth release first and no admission pause:
// the profile verifies under its digest name, and the proof binds its digest.
func TestNonrootVerifierFollowsTheRunningProfile(t *testing.T) {
	profile, err := argoworkflow.NewNonrootProfile(4, 2)
	if err != nil {
		t.Fatal(err)
	}
	pod, cm := nonrootVerifierObjects()
	withProfile(pod, cm, profile)
	proof, err := verifyNonrootController(t.Context(), fake.NewClientset(pod, cm), testNamespace, "", testRequest(pipeline.TriggerCI, nonrootTestDocument))
	if err != nil || proof.configSHA256 != profile.SHA256 {
		t.Fatalf("followed profile = %+v, %v", proof, err)
	}
	// A profile name whose ConfigMap is missing is refused, not replaced by
	// the built-in one.
	if _, err := verifyNonrootController(t.Context(), fake.NewClientset(pod), testNamespace, "", testRequest(pipeline.TriggerCI, nonrootTestDocument)); err == nil {
		t.Fatal("missing followed profile ConfigMap accepted")
	}
	// A --configmap that is not a profile name falls back to the built-in
	// name, which the exact argument check then refuses.
	other, otherCM := nonrootVerifierObjects()
	other.Spec.Containers[0].Args[1] = "workflow-controller-configmap"
	if _, err := verifyNonrootController(t.Context(), fake.NewClientset(other, otherCM), testNamespace, "", testRequest(pipeline.TriggerCI, nonrootTestDocument)); err == nil {
		t.Fatal("controller on a non-profile ConfigMap accepted")
	}
}
