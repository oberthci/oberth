package installer

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestCapacityChecksChildNamespaceInEveryInterval(t *testing.T) {
	for _, tc := range []struct {
		name          string
		covered, fits bool
	}{
		{"small child quota without worst coverage", false, false},
		{"sufficient child quota without worst coverage", false, true},
		{"small child quota with worst coverage", true, false},
		{"sufficient child quota with worst coverage", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := capacityFixture("2100m", "6Gi", 1)
			child := *e.Quotas[0].DeepCopy()
			child.Namespace = "children"
			shares := map[corev1.ResourceName][2]string{
				corev1.ResourceRequestsCPU:              {"8900m", "100m"},
				corev1.ResourceRequestsMemory:           {"47Gi", "1Gi"},
				corev1.ResourceLimitsMemory:             {"47Gi", "1Gi"},
				corev1.ResourceRequestsEphemeralStorage: {"240Gi", "16Gi"},
				corev1.ResourceLimitsEphemeralStorage:   {"240Gi", "16Gi"},
				corev1.ResourcePods:                     {"79", "1"},
			}
			if tc.fits {
				shares[corev1.ResourceRequestsCPU] = [2]string{"8500m", "500m"}
				shares[corev1.ResourceRequestsMemory] = [2]string{"40Gi", "8Gi"}
				shares[corev1.ResourceLimitsMemory] = [2]string{"40Gi", "8Gi"}
				shares[corev1.ResourceRequestsEphemeralStorage] = [2]string{"192Gi", "64Gi"}
				shares[corev1.ResourceLimitsEphemeralStorage] = [2]string{"192Gi", "64Gi"}
				shares[corev1.ResourcePods] = [2]string{"76", "4"}
			}
			for name, pair := range shares {
				e.Quotas[0].Spec.Hard[name] = resource.MustParse(pair[0])
				child.Spec.Hard[name] = resource.MustParse(pair[1])
			}
			e.Quotas = append(e.Quotas, child)
			pod := capacityPod("child", "100m", "1Gi")
			pod.Namespace = "children"
			e.Families[0].Intervals[0].Seconds = 95
			e.Families[0].Intervals = append(e.Families[0].Intervals, CapacityInterval{Seconds: 5, Pods: []corev1.Pod{pod}})
			if tc.covered {
				e.Families[0].WorstPods = append(e.Families[0].WorstPods, *pod.DeepCopy())
			}
			a := AdviseCapacity(e)
			if tc.covered && tc.fits {
				if a.Parallel == nil || *a.Parallel != 4 {
					t.Fatal("valid multi-namespace phase evidence lost cap 4", a)
				}
			} else if a.Parallel != nil {
				t.Fatal("smaller child phase bypassed namespace coverage or quota", *a.Parallel)
			}
		})
	}
}
