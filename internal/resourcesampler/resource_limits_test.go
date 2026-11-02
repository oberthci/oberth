package resourcesampler

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestResourceHistoryCapturesBoundNumericLimitsAndTemplate(t *testing.T) {
	b, p, s, at := sampleFixture(t)
	b.TemplateName = "frag-12345678-step-abc12345"
	b.FragmentRepository = "repo-01"
	p.Spec.Containers[0].Resources.Limits = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("1Gi"), corev1.ResourceEphemeralStorage: resource.MustParse("4Gi")}
	got, ok := Observe(b, p, s, at)
	if !ok || got.TemplateName != b.TemplateName || got.FragmentRepository != b.FragmentRepository || got.CPULimitNanoCores == nil || *got.CPULimitNanoCores != 2e9 || got.MemoryLimitBytes == nil || *got.MemoryLimitBytes != 1<<30 || got.EphemeralLimitBytes == nil || *got.EphemeralLimitBytes != 4<<30 {
		t.Fatalf("provenance: %+v", got)
	}
}
