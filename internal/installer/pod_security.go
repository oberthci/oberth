package installer

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
)

const podSecurityEnforce = "pod-security.kubernetes.io/enforce"

// VerifyPipelineNamespace checks the live enforcement label without changing it.
func VerifyPipelineNamespace(ctx context.Context, namespaces typedcorev1.NamespaceInterface, name string) error {
	ns, err := namespaces.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return described("cluster", err, "Pipeline namespace: "+name+"; "+err.Error())
	}
	return pipelinePodSecurity(ns)
}

func pipelinePodSecurity(ns *corev1.Namespace) error {
	level := ns.Labels[podSecurityEnforce]
	if level != "baseline" && level != "restricted" {
		return described("pipeline-security", nil, "Pipeline namespace: "+ns.Name)
	}
	return nil
}
