package argojob

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
)

// Used only by Oberth-created helper containers, including the executor-token waiter.
func serverContainerSecurity(uid int64) *corev1.SecurityContext {
	return &corev1.SecurityContext{
		RunAsUser: ptr.To(uid), RunAsGroup: ptr.To(uid), RunAsNonRoot: ptr.To(uid != 0),
		AllowPrivilegeEscalation: ptr.To(false), ReadOnlyRootFilesystem: ptr.To(true),
		Capabilities:   &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}
