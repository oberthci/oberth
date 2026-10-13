package argojob

import (
	"errors"
	"fmt"
	"strings"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/oberthci/oberth/internal/vmrunner"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

// ErrTrustedTestsNotEnabled is the sentinel for the infrastructure-class
// failure when a tests.yaml v2 contract exists but the server switch is off.
var ErrTrustedTestsNotEnabled = errors.New("argojob: trusted test contract exists but vm.trustedTests.enabled is false")

// appendTrustedTestsLeaf adds the server-controlled trusted-tests leaf to the
// workflow when the request carries a resolved v2 contract. The leaf's image,
// command, and args are entirely server-owned; the repository can request a
// trusted test by declaring a profile in tests.yaml but cannot author the leaf.
//
// This runs after all other per-template rewrites so the trusted leaf is not
// subject to workspace injection, nonroot rewriting, or KVM leaf processing.
func appendTrustedTestsLeaf(workflow *wfv1.Workflow, config Config, request Request) error {
	if request.TrustedTestContract == nil || request.TrustedTestContract.Version < 2 {
		return nil
	}

	if !config.TrustedTestsEnabled {
		return fmt.Errorf("%w: profile %s requires a trusted executor; enable vm.trustedTests in the chart",
			ErrTrustedTestsNotEnabled, request.TrustedTestContract.Profile)
	}

	if strings.TrimSpace(config.TrustedTestsHarnessImage) == "" {
		return errors.New("argojob: vm.trustedTests.enabled is true but no harness image is configured (vm.trustedTests.harnessImage)")
	}

	// Validate the harness image is digest-pinned.
	if !strings.Contains(config.TrustedTestsHarnessImage, "@sha256:") {
		return fmt.Errorf("argojob: trusted tests harness image %q must be pinned by digest (@sha256:...)", config.TrustedTestsHarnessImage)
	}

	contract := request.TrustedTestContract
	templateName := vmrunner.BurnName

	// Check the template name is not already used.
	for _, tmpl := range workflow.Spec.Templates {
		if tmpl.Name == templateName {
			return fmt.Errorf("argojob: workflow already contains a template named %q; the trusted-tests leaf name is server-reserved", templateName)
		}
	}

	falseVal := false
	trustedTemplate := wfv1.Template{
		Name: templateName,
		Metadata: wfv1.Metadata{
			Annotations: map[string]string{
				argoworkflow.WorkspaceMountsAnnotation: "none",
				argoworkflow.WorkspaceEnvAnnotation:    "none",
			},
		},
		AutomountServiceAccountToken: &falseVal,
		Container: &corev1.Container{
			Image:   config.TrustedTestsHarnessImage,
			Command: []string{"/harness"},
			Args: []string{
				"--profile=" + contract.Profile,
				"--artifact=" + contract.Artifact,
				"--candidate-sha=" + request.SHA,
				"--suite-identity=" + request.TrustedTestSuiteIdentity,
				"--guest-image=" + contract.GuestImage,
			},
			Env: []corev1.EnvVar{
				{Name: "OBERTH_TRUSTED_TEST", Value: "1"},
				{Name: "OBERTH_PROFILE", Value: contract.Profile},
				{Name: "OBERTH_CANDIDATE_SHA", Value: request.SHA},
				{Name: "OBERTH_SUITE_IDENTITY", Value: request.TrustedTestSuiteIdentity},
				{Name: "OBERTH_GUEST_IMAGE", Value: contract.GuestImage},
			},
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse(fmt.Sprintf("%d", contract.Resources.CPUCores)),
					corev1.ResourceMemory: resource.MustParse(fmt.Sprintf("%dMi", contract.Resources.MemoryMiB)),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse(fmt.Sprintf("%d", contract.Resources.CPUCores)),
					corev1.ResourceMemory: resource.MustParse(fmt.Sprintf("%dMi", contract.Resources.MemoryMiB)),
				},
			},
			TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
		},
	}

	// If KVM is enabled, inject the device resource (same pattern as Lane A).
	if config.KVMEnabled {
		kvmQuantity := resource.MustParse("1")
		trustedTemplate.Container.Resources.Requests[corev1.ResourceName("devices.kubevirt.io/kvm")] = kvmQuantity
		trustedTemplate.Container.Resources.Limits[corev1.ResourceName("devices.kubevirt.io/kvm")] = kvmQuantity
		trustedTemplate.Container.Env = append(trustedTemplate.Container.Env,
			corev1.EnvVar{Name: "OBERTH_KVM", Value: "1"})
	}

	// Append the template.
	workflow.Spec.Templates = append(workflow.Spec.Templates, trustedTemplate)

	// Wire it into the DAG: find the entrypoint template and add a task
	// that depends on all existing tasks completing.
	entrypoint := findEntrypointTemplate(workflow)
	if entrypoint != nil && entrypoint.DAG != nil {
		existingTasks := make([]string, 0, len(entrypoint.DAG.Tasks))
		for _, task := range entrypoint.DAG.Tasks {
			existingTasks = append(existingTasks, task.Name)
		}
		entrypoint.DAG.Tasks = append(entrypoint.DAG.Tasks, wfv1.DAGTask{
			Name:         templateName,
			Template:     templateName,
			Dependencies: existingTasks,
		})
	}

	return nil
}

// findEntrypointTemplate returns the workflow's entrypoint template.
func findEntrypointTemplate(workflow *wfv1.Workflow) *wfv1.Template {
	entrypoint := workflow.Spec.Entrypoint
	if entrypoint == "" {
		return nil
	}
	for i := range workflow.Spec.Templates {
		if workflow.Spec.Templates[i].Name == entrypoint {
			return &workflow.Spec.Templates[i]
		}
	}
	return nil
}
