package resourcesampler

import (
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestMetricCoverageDoesNotBorrowOtherMetricTimestamps(t *testing.T) {
	b, pod, snapshot, at := storageFixture(t)
	v, ok := Observe(b, pod, snapshot, at)
	if !ok {
		t.Fatal("fixture not observed")
	}
	raw, _ := json.Marshal(v)
	var doc map[string]json.RawMessage
	_ = json.Unmarshal(raw, &doc)
	var storage struct {
		Count int64 `json:"count"`
	}
	if field := doc["storage_coverage"]; field != nil {
		_ = json.Unmarshal(field, &storage)
	}
	// This fixture has PVC/rootfs/CPU statistics but no local-ephemeral metric.
	if storage.Count != 0 {
		t.Fatalf("unobserved storage acquired coverage: %s", raw)
	}
	var memory struct {
		Count int64 `json:"count"`
	}
	if err := json.Unmarshal(doc["memory_coverage"], &memory); err != nil || memory.Count != 1 {
		t.Fatalf("main memory source coverage absent: %s", raw)
	}
}

func TestPodLocalLimitUsesEffectiveInitAndHelperScope(t *testing.T) {
	b, pod, snapshot, at := sampleFixture(t)
	pod.Spec.Containers[0].Resources.Limits = corev1.ResourceList{corev1.ResourceEphemeralStorage: resource.MustParse("64Mi")}
	pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{Name: "wait", Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceEphemeralStorage: resource.MustParse("32Mi")}}})
	pod.Spec.InitContainers = []corev1.Container{{Name: "init", Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceEphemeralStorage: resource.MustParse("128Mi")}}}}
	v, ok := Observe(b, pod, snapshot, at)
	if !ok || v.PodEphemeralLimitBytes == nil || *v.PodEphemeralLimitBytes != 128<<20 || *v.EphemeralLimitBytes != 64<<20 {
		t.Fatalf("main and effective Pod scope mixed: %+v", v)
	}
}
