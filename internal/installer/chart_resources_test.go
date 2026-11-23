package installer_test

import (
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestControlPodResourceDefaults(t *testing.T) {
	t.Parallel()
	objects := rendered(t, nil)
	for _, tc := range []struct {
		kind, name, cpu, memory, limit string
	}{
		{"Deployment", "oberth", "250m", "1Gi", ""},
		{"Deployment", "oberth-controller-workflow-controller", "100m", "512Mi", ""},
		{"StatefulSet", "oberth-store", "50m", "128Mi", "512Mi"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			template := object(t, objects, tc.kind, tc.name)["spec"].(map[string]any)["template"]
			var pod corev1.PodTemplateSpec
			if err := json.Unmarshal([]byte(jsonOf(t, template)), &pod); err != nil {
				t.Fatal(err)
			}
			if len(pod.Spec.Containers) != 1 {
				t.Fatal("unexpected control Pod containers", pod.Spec.Containers)
			}
			r := pod.Spec.Containers[0].Resources
			if r.Requests.Cpu().Cmp(resource.MustParse(tc.cpu)) != 0 || r.Requests.Memory().Cmp(resource.MustParse(tc.memory)) != 0 {
				t.Fatal("control Pod requests differ", r)
			}
			if !r.Limits.Cpu().IsZero() {
				t.Fatal("control Pod CPU bursts are capped", r)
			}
			assertControlMemoryLimit(t, r, tc.limit)
		})
	}
}

func assertControlMemoryLimit(t *testing.T, r corev1.ResourceRequirements, limit string) {
	t.Helper()
	if limit == "" {
		if !r.Limits.Memory().IsZero() {
			t.Fatal("control Pod memory is capped", r)
		}
	} else if r.Limits.Memory().Cmp(resource.MustParse(limit)) != 0 {
		t.Fatal("control Pod memory limit differs", r)
	}
}
