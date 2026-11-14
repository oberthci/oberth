package installer

import (
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
)

func argoWorkflowPodSecurity(values map[string]any) (*corev1.PodSecurityContext, error) {
	raw, err := json.Marshal(valueAt(values, "controller", "workflowDefaults", "spec", "securityContext"))
	if err != nil {
		return nil, err
	}
	var security *corev1.PodSecurityContext
	if err := json.Unmarshal(raw, &security); err != nil {
		return nil, fmt.Errorf("decode controller.workflowDefaults.spec.securityContext: %w", err)
	}
	return security, nil
}

func argoPodSecurityContextMeetsFloor(security *corev1.PodSecurityContext) bool {
	return security == nil || (allowedArgoSeccomp(security.SeccompProfile) &&
		(security.WindowsOptions == nil || security.WindowsOptions.HostProcess == nil || !*security.WindowsOptions.HostProcess))
}

func allowedArgoSeccomp(profile *corev1.SeccompProfile) bool {
	return profile == nil || profile.Type == corev1.SeccompProfileTypeRuntimeDefault || profile.Type == corev1.SeccompProfileTypeLocalhost
}

func argoWorkflowSecurityContextMeetsFloor(security *corev1.SecurityContext, pod *corev1.PodSecurityContext) bool {
	if security == nil {
		return false
	}
	// Kubernetes inherits a missing container seccomp profile from the Pod.
	// This copy is used only for validation; native Helm values stay unchanged.
	effective := security.DeepCopy()
	if effective.SeccompProfile == nil && pod != nil {
		effective.SeccompProfile = pod.SeccompProfile.DeepCopy()
	}
	return argoSecurityContextMeetsFloor(effective, true)
}

func validateArgoWorkflowPodSecurity(location string, security *corev1.PodSecurityContext) error {
	if argoPodSecurityContextMeetsFloor(security) {
		return nil
	}
	if !allowedArgoSeccomp(security.SeccompProfile) {
		return fmt.Errorf("%s.seccompProfile.type=%q cannot pass the Pod policy; set this native Pod default to RuntimeDefault or Localhost", location, security.SeccompProfile.Type)
	}
	return fmt.Errorf("%s.windowsOptions.hostProcess=true cannot pass the Pod policy; set it to false", location)
}

func validatePlannedArgoWorkflowSecurity(namespace string, stored map[string]any, review []ArgoSecurityReview) error {
	pod, err := argoWorkflowPodSecurity(stored)
	if err != nil {
		return argoSecurityUpgradeRemedy(namespace, err)
	}
	for _, entry := range review {
		if entry.Component != "mainContainer" && entry.Component != "executor" {
			continue
		}
		security, _ := entry.Planned.(*corev1.SecurityContext)
		if !argoWorkflowSecurityContextMeetsFloor(security, pod) {
			return argoSecurityUpgradeRemedy(namespace, fmt.Errorf("%s.securityContext requires effective RuntimeDefault or Localhost seccomp, directly or through controller.workflowDefaults.spec.securityContext", entry.Component))
		}
	}
	return nil
}
