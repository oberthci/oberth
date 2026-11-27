package installer

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestCapacityDeclinesEffectiveUnsupportedResources(t *testing.T) {
	for _, source := range []string{"pod requests", "pod limits", "pod requests and limits", "container requests", "container limits", "init", "overhead"} {
		t.Run(source, func(t *testing.T) {
			e := capacityFixture("2100m", "6Gi", 1)
			for _, pods := range [][]corev1.Pod{e.Families[0].WorstPods, e.Families[0].Intervals[0].Pods} {
				pod := &pods[0]
				pod.Spec.Resources = &corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("6Gi")},
					Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("6Gi")},
				}
				var list corev1.ResourceList
				switch source {
				case "pod requests":
					list = pod.Spec.Resources.Requests
				case "pod limits":
					list = pod.Spec.Resources.Limits
				case "pod requests and limits":
					list = pod.Spec.Resources.Requests
					pod.Spec.Resources.Limits[corev1.ResourceName("hugepages-2Mi")] = resource.MustParse("80Mi")
				case "container requests":
					list = pod.Spec.Containers[0].Resources.Requests
				case "container limits":
					list = pod.Spec.Containers[0].Resources.Limits
				case "init":
					pod.Spec.InitContainers = []corev1.Container{*pod.Spec.Containers[0].DeepCopy()}
					list = pod.Spec.InitContainers[0].Resources.Requests
				case "overhead":
					pod.Spec.Overhead = corev1.ResourceList{}
					list = pod.Spec.Overhead
				}
				list[corev1.ResourceName("hugepages-2Mi")] = resource.MustParse("80Mi")
			}
			if a := AdviseCapacity(e); a.Parallel != nil {
				t.Fatal("unsupported effective hugepages produced advice", *a.Parallel)
			}
		})
	}
}

func TestCapacityAcceptsSupportedPodLevelResources(t *testing.T) {
	e := capacityFixture("2100m", "6Gi", 1)
	for _, pods := range [][]corev1.Pod{e.Families[0].WorstPods, e.Families[0].Intervals[0].Pods} {
		pods[0].Spec.Resources = &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("6Gi")},
			Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("6Gi")},
		}
	}
	if a := AdviseCapacity(e); a.Parallel == nil || *a.Parallel != 4 {
		t.Fatal("supported native Pod resources lost cap 4", a)
	}
}
