package argoworkflow

import (
	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func declaredLeaf(name, cpu, memory string) wfv1.Template {
	return wfv1.Template{Name: name, Container: &corev1.Container{Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse(memory)}, Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(memory)}}}}
}
