package installer_test

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	psapi "k8s.io/pod-security-admission/api"
	"k8s.io/pod-security-admission/policy"
)

func TestNewServerNamespaceEnforcesRestrictedPodSecurity(t *testing.T) {
	f := setup(t)
	apply(t, f, prepare(t, f))
	ns, err := f.client.CoreV1().Namespaces().Get(t.Context(), "oberth", metav1.GetOptions{})
	if err != nil || ns.Labels["pod-security.kubernetes.io/enforce"] != "restricted" {
		t.Fatal("new server namespace is not restricted", err)
	}
	evaluator, err := policy.NewEvaluator(policy.DefaultChecks(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var count int
	for _, obj := range rendered(t, nil) {
		var pod corev1.PodTemplateSpec
		switch obj["kind"] {
		case "Deployment":
			var deployment appsv1.Deployment
			decodeObject(t, obj, &deployment)
			pod = deployment.Spec.Template
		case "StatefulSet":
			var stateful appsv1.StatefulSet
			decodeObject(t, obj, &stateful)
			pod = stateful.Spec.Template
		default:
			continue
		}
		result := policy.AggregateCheckResults(evaluator.EvaluatePod(psapi.LevelVersion{Level: psapi.LevelRestricted, Version: psapi.LatestVersion()}, &pod.ObjectMeta, &pod.Spec))
		if !result.Allowed {
			t.Fatal("system Pod fails restricted admission", obj["metadata"], result.ForbiddenDetail())
		}
		count++
	}
	if count != 3 {
		t.Fatal("server, controller and store were not checked", count)
	}
}
