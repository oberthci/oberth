package installer_test

import (
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestControlChoicesAcceptNativeNumericQuantities(t *testing.T) {
	values, err := installer.ResourceChoices(`{"server":{"requests":{"cpu":1,"memory":1073741824}}}`)
	if err != nil {
		t.Fatal(err)
	}
	objects := rendered(t, values)
	var pod corev1.PodTemplateSpec
	template := object(t, objects, "Deployment", "oberth")["spec"].(map[string]any)["template"].(map[string]any)
	decodeObject(t, template, &pod)
	r := pod.Spec.Containers[0].Resources.Requests
	if r.Cpu().Cmp(resource.MustParse("1")) != 0 || r.Memory().Cmp(resource.MustParse("1Gi")) != 0 {
		t.Fatal("native numeric quantities changed", r)
	}
}

func TestRunDefaultsAcceptNativeExponentQuantities(t *testing.T) {
	values := runPolicyFixture()
	policy := values["runPolicy"].(map[string]any)
	delete(policy["namespaces"].(map[string]any), "children")
	policy["defaultRequest"] = map[string]any{"cpu": "100e-3"}
	objects := rendered(t, values)
	var defaults corev1.LimitRange
	decodeObject(t, object(t, objects, "LimitRange", "oberth-run-defaults"), &defaults)
	if defaults.Spec.Limits[0].DefaultRequest.Cpu().Cmp(resource.MustParse("100m")) != 0 {
		t.Fatal("native exponent quantity changed", defaults)
	}
}
