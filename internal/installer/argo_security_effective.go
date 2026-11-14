package installer

import (
	"encoding/json"
	"fmt"
	"strings"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

func argoWorkflowDefaultsSpec(values map[string]any) (*wfv1.WorkflowSpec, error) {
	raw, err := json.Marshal(valueAt(values, "controller", "workflowDefaults", "spec"))
	if err != nil {
		return nil, err
	}
	var spec *wfv1.WorkflowSpec
	if err := json.Unmarshal(raw, &spec); err != nil {
		return nil, fmt.Errorf("decode controller.workflowDefaults.spec: %w", err)
	}
	return spec, nil
}

// Argo v4.0.8 workflowpod.go uses the template Pod context instead of the
// workflow context, even when the template context has no seccomp profile.
func effectiveArgoWorkflowPodSecurity(spec *wfv1.WorkflowSpec) (*corev1.PodSecurityContext, string) {
	if spec == nil {
		return nil, "securityContext"
	}
	if spec.TemplateDefaults != nil && spec.TemplateDefaults.SecurityContext != nil {
		return spec.TemplateDefaults.SecurityContext, "templateDefaults.securityContext"
	}
	return spec.SecurityContext, "securityContext"
}

func argoWorkflowPodSecurity(values map[string]any) (*corev1.PodSecurityContext, error) {
	spec, err := argoWorkflowDefaultsSpec(values)
	if err != nil {
		return nil, err
	}
	pod, _ := effectiveArgoWorkflowPodSecurity(spec)
	return pod, nil
}

type argoWorkflowDefaultsInvalid struct{ message string }

func (e *argoWorkflowDefaultsInvalid) Error() string { return e.message }

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
		return &argoWorkflowDefaultsInvalid{fmt.Sprintf("%s.seccompProfile.type=%q violates the Argo security floor; set this native Pod default to RuntimeDefault or Localhost", location, security.SeccompProfile.Type)}
	}
	return &argoWorkflowDefaultsInvalid{fmt.Sprintf("%s.windowsOptions.hostProcess=true violates the Argo security floor; set it to false", location)}
}

func validateArgoWorkflowDefaults(location string, spec *wfv1.WorkflowSpec) error {
	pod, key := effectiveArgoWorkflowPodSecurity(spec)
	if err := validateArgoWorkflowPodSecurity(location+"."+key, pod); err != nil {
		return err
	}
	if spec == nil {
		return nil
	}
	if err := validateArgoSeccompPatch(location+".podSpecPatch", spec.PodSpecPatch); err != nil {
		return err
	}
	tmpl := spec.TemplateDefaults
	if tmpl == nil {
		return nil
	}
	if err := validateArgoSeccompPatch(location+".templateDefaults.podSpecPatch", tmpl.PodSpecPatch); err != nil {
		return err
	}
	contexts := map[string]*corev1.SecurityContext{}
	if tmpl.Container != nil {
		contexts["container"] = tmpl.Container.SecurityContext
	}
	if tmpl.Script != nil {
		contexts["script"] = tmpl.Script.SecurityContext
	}
	for _, c := range tmpl.InitContainers {
		contexts["initContainers."+c.Name] = c.SecurityContext
	}
	for _, c := range tmpl.Sidecars {
		contexts["sidecars."+c.Name] = c.SecurityContext
	}
	if tmpl.ContainerSet != nil {
		for _, c := range tmpl.ContainerSet.Containers {
			contexts["containerSet.containers."+c.Name] = c.SecurityContext
		}
	}
	for key, security := range contexts {
		if security != nil && !allowedArgoSeccomp(security.SeccompProfile) {
			return &argoWorkflowDefaultsInvalid{fmt.Sprintf("%s.templateDefaults.%s.securityContext.seccompProfile.type=%q violates the Argo security floor; set it to RuntimeDefault or Localhost", location, key, security.SeccompProfile.Type)}
		}
	}
	return nil
}

func validateArgoSeccompPatch(location, patch string) error {
	if strings.TrimSpace(patch) == "" {
		return nil
	}
	var value any
	if err := yaml.Unmarshal([]byte(patch), &value); err != nil {
		return &argoWorkflowDefaultsInvalid{location + " cannot be parsed to verify effective seccomp; remove it or configure securityContext in native defaults"}
	}
	if argoPatchChangesSeccomp(value) {
		return &argoWorkflowDefaultsInvalid{location + " can change effective seccomp; remove the securityContext or replacement from this patch and configure native security defaults directly"}
	}
	return nil
}

func argoPatchChangesSeccomp(value any) bool {
	switch v := value.(type) {
	case map[string]any:
		for key, child := range v {
			if key == "securityContext" || key == "$patch" || key == "$retainKeys" || strings.Contains(key, "{{") {
				return true
			}
			if argoPatchChangesSeccomp(child) {
				return true
			}
		}
	case []any:
		for _, child := range v {
			if argoPatchChangesSeccomp(child) {
				return true
			}
		}
	}
	return false
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
