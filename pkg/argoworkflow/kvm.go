package argoworkflow

import (
	"errors"
	"fmt"
	"strings"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	k8svalidation "k8s.io/apimachinery/pkg/util/validation"
)

// KVMTemplatesAnnotation selects named leaves for server-injected KVM device
// access. The server adds `devices.kubevirt.io/kvm: "1"` (request == limit)
// and `OBERTH_KVM=1` to each listed leaf's container. The repository document
// must NEVER declare `devices.kubevirt.io/*` resources itself -- admission
// refuses them outright (admit.go:admitContainerResources) -- because the
// device plugin allocation is a server-owned infrastructure decision, not a
// repository capability.
//
// This is Lane A (coverage acceleration): ordinary trust model, same pod,
// same deadline, kubelet exit code. It is NOT Lane B (trusted suites /
// tests.yaml v2 / trusted_policy).
const KVMTemplatesAnnotation = "oberth.ci/kvm-templates"

// DeclaredKVMTemplates validates the annotation and returns the declared leaf
// names. Each named leaf must be a plain container or script template with:
//   - workspace-mounts: none AND workspace-env: none
//   - automountServiceAccountToken: false
//   - no templateDefaults on the workflow
//   - not also named in oberth.ci/nonroot-templates
//   - not a WIF/credentialed template (no release-wif-role annotation, no
//     secretstore exec, no secret-paths)
//   - no repo-declared devices.kubevirt.io/* resource anywhere in the workflow
//   - inputs.parameters ARE allowed (unlike nonroot)
//
// Unknown leaf name -> admission error naming it.
func DeclaredKVMTemplates(workflow *wfv1.Workflow) ([]string, error) {
	if workflow == nil {
		return nil, errors.New("argoworkflow: no Workflow to read")
	}
	value, declared := workflow.Annotations[KVMTemplatesAnnotation]
	if !declared {
		return nil, nil
	}
	fields := strings.Split(value, ",")
	if len(fields) > MaxTemplates {
		return nil, fmt.Errorf("argoworkflow: %s exceeds %d templates",
			KVMTemplatesAnnotation, MaxTemplates)
	}

	// KVM leaves cannot coexist with credentialed workflows.
	paths, err := DeclaredSecretPaths(workflow)
	if err != nil {
		return nil, err
	}
	if len(paths) != 0 {
		return nil, errors.New("argoworkflow: KVM leaves are not admitted in credentialed workflows")
	}

	// No templateDefaults: the server must enumerate every container and mount
	// on each leaf without late controller merging changing the shape.
	if workflow.Spec.TemplateDefaults != nil {
		return nil, errors.New("argoworkflow: KVM leaves cannot inherit templateDefaults")
	}

	// No repo-declared KVM device resource anywhere in the workflow: the device
	// is exclusively server-injected. admitContainerResources already refuses
	// unknown resources, but this is defense in depth for the specific resource.
	if err := rejectRepoKVMDevices(workflow); err != nil {
		return nil, err
	}

	// Build the nonroot set to check for overlap.
	nonrootSet := map[string]bool{}
	if nonrootValue, ok := workflow.Annotations[NonrootTemplatesAnnotation]; ok {
		for _, name := range strings.Split(nonrootValue, ",") {
			nonrootSet[strings.TrimSpace(name)] = true
		}
	}

	// Build the WIF set to check for credentialed overlap.
	wifSet := map[string]bool{}
	for _, tmpl := range workflow.Spec.Templates {
		if _, hasWIF := tmpl.Metadata.Annotations[ReleaseWIFRoleAnnotation]; hasWIF {
			wifSet[tmpl.Name] = true
		}
	}

	templates := make(map[string]*wfv1.Template, len(workflow.Spec.Templates))
	for i := range workflow.Spec.Templates {
		templates[workflow.Spec.Templates[i].Name] = &workflow.Spec.Templates[i]
	}

	seen := make(map[string]bool, len(fields))
	for i, field := range fields {
		name := strings.TrimSpace(field)
		if len(k8svalidation.IsDNS1123Label(name)) != 0 || seen[name] {
			return nil, fmt.Errorf("argoworkflow: %s has invalid or duplicate template %q",
				KVMTemplatesAnnotation, name)
		}

		tmpl := templates[name]
		if tmpl == nil {
			return nil, fmt.Errorf("argoworkflow: %s names unknown top-level template %q",
				KVMTemplatesAnnotation, name)
		}

		// Must be a plain container or script leaf -- no DAG, steps, suspend,
		// containerSet, resource, data, plugin, HTTP, sidecars, init containers.
		if (tmpl.Container == nil) == (tmpl.Script == nil) || tmpl.ContainerSet != nil ||
			tmpl.DAG != nil || len(tmpl.Steps) != 0 || len(tmpl.InitContainers) != 0 ||
			len(tmpl.Sidecars) != 0 || tmpl.Daemon != nil || tmpl.Resource != nil ||
			tmpl.Data != nil || tmpl.Suspend != nil || tmpl.HTTP != nil || tmpl.Plugin != nil {
			return nil, fmt.Errorf("argoworkflow: KVM template %q must be a plain container or script leaf",
				name)
		}

		// Must have workspace-mounts: none AND workspace-env: none.
		if tmpl.Metadata.Annotations[WorkspaceMountsAnnotation] != "none" {
			return nil, fmt.Errorf("argoworkflow: KVM template %q requires %s: none",
				name, WorkspaceMountsAnnotation)
		}
		if tmpl.Metadata.Annotations[WorkspaceEnvAnnotation] != "none" {
			return nil, fmt.Errorf("argoworkflow: KVM template %q requires %s: none",
				name, WorkspaceEnvAnnotation)
		}

		// Must have automountServiceAccountToken: false.
		if tmpl.AutomountServiceAccountToken == nil || *tmpl.AutomountServiceAccountToken {
			return nil, fmt.Errorf("argoworkflow: KVM template %q must set automountServiceAccountToken: false",
				name)
		}

		// Must not overlap with nonroot templates.
		if nonrootSet[name] {
			return nil, fmt.Errorf("argoworkflow: KVM template %q is also named in %s; a template cannot be both",
				name, NonrootTemplatesAnnotation)
		}

		// Must not be a WIF/credentialed template.
		if wifSet[name] {
			return nil, fmt.Errorf("argoworkflow: KVM template %q declares a release WIF role; credentialed leaves cannot be KVM leaves",
				name)
		}

		fields[i], seen[name] = name, true
	}
	return fields, nil
}

// rejectRepoKVMDevices refuses any repo-declared devices.kubevirt.io/*
// resource in any container of the workflow. admitContainerResources already
// refuses unknown resources generically, but this provides a specific,
// actionable error for the KVM case and catches it even when the KVM
// annotation names only a subset of templates.
func rejectRepoKVMDevices(workflow *wfv1.Workflow) error {
	for i := range workflow.Spec.Templates {
		tmpl := &workflow.Spec.Templates[i]
		location := fmt.Sprintf("spec.templates[%q]", tmpl.Name)
		if tmpl.Name == "" {
			location = fmt.Sprintf("spec.templates[%d]", i)
		}
		if tmpl.Container != nil {
			if err := checkContainerKVM(location+".container", tmpl.Container); err != nil {
				return err
			}
		}
		if tmpl.Script != nil {
			if err := checkContainerKVM(location+".script", &tmpl.Script.Container); err != nil {
				return err
			}
		}
		if tmpl.ContainerSet != nil {
			for j := range tmpl.ContainerSet.Containers {
				if err := checkContainerKVM(
					fmt.Sprintf("%s.containerSet.containers[%d]", location, j),
					&tmpl.ContainerSet.Containers[j].Container); err != nil {
					return err
				}
			}
		}
		for j := range tmpl.InitContainers {
			if err := checkContainerKVM(
				fmt.Sprintf("%s.initContainers[%d]", location, j),
				&tmpl.InitContainers[j].Container); err != nil {
				return err
			}
		}
		for j := range tmpl.Sidecars {
			if err := checkContainerKVM(
				fmt.Sprintf("%s.sidecars[%d]", location, j),
				&tmpl.Sidecars[j].Container); err != nil {
				return err
			}
		}
	}
	return nil
}

func checkContainerKVM(location string, c *corev1.Container) error {
	if c == nil {
		return nil
	}
	for name := range c.Resources.Requests {
		if strings.HasPrefix(string(name), "devices.kubevirt.io/") {
			return fmt.Errorf("argoworkflow: %s declares resource %q; KVM device resources are server-injected, not repository-declared",
				location, name)
		}
	}
	for name := range c.Resources.Limits {
		if strings.HasPrefix(string(name), "devices.kubevirt.io/") {
			return fmt.Errorf("argoworkflow: %s declares resource %q; KVM device resources are server-injected, not repository-declared",
				location, name)
		}
	}
	return nil
}
