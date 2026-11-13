package installer

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// UIDs come only from successful Pod creation responses and stay fixed for the
// measurement. A familiar name, label or address does not authorize a replacement.
type policyProbePods struct {
	namespace string
	names     []string
	uids      map[string]types.UID
}

func (pods policyProbePods) get(ctx context.Context, deps Deps, name string) (*corev1.Pod, error) {
	uid, exists := pods.uids[name]
	if !exists || uid == "" || deps.KubeClient == nil {
		return nil, &policyTrafficError{detail: "Probe Pod identity is unavailable."}
	}
	pod, err := deps.KubeClient.CoreV1().Pods(pods.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if pod.UID != uid || pod.DeletionTimestamp != nil {
		return nil, &policyTrafficError{detail: "Probe Pod identity changed for " + name + "."}
	}
	return pod, nil
}
