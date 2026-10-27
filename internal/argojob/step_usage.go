package argojob

import (
	"slices"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"

	"github.com/oberthci/oberth/internal/stepusage"
)

const usageBinMountPath = "/run/oberth/usage"
const usageBinPath = usageBinMountPath + "/oberth"
const usageCommand = "internal-step-exec"

// Only explicit commands can be wrapped without changing an image's ENTRYPOINT.
// Script commands retain Argo's appended script path. ContainerSets/resource
// executors and opaque image entrypoints remain unknown, never fabricated zero.
func usageContainer(t *wfv1.Template) *corev1.Container {
	if t.Container != nil {
		return t.Container
	}
	if t.Script != nil {
		return &t.Script.Container
	}
	return nil
}

func injectStepUsageMounts(wf *wfv1.Workflow, request Request) {
	if request.omitStepUsage || request.TrustedKVM != nil || !request.binaryDelivered() || request.SourceVolume.ClaimName == "" {
		return
	}
	walkTemplates(wf, func(t *wfv1.Template) {
		c := usageContainer(t)
		if c == nil || len(c.Command) == 0 || (c.TerminationMessagePath != "" && c.TerminationMessagePath != stepusage.TerminationPath) {
			return
		}
		// Replace any repository collision with this server-owned, read-only subpath.
		mounts := c.VolumeMounts[:0:0]
		for _, m := range c.VolumeMounts {
			if m.MountPath != usageBinMountPath {
				mounts = append(mounts, m)
			}
		}
		c.VolumeMounts = append(mounts, corev1.VolumeMount{Name: SourceVolumeName, MountPath: usageBinMountPath, SubPath: request.SourceVolume.BinarySubPath, ReadOnly: true})
	})
}

// Wrap last: credential-chain and executor-token admission inspect the original
// command. This transformation adds no credentials and changes no security context.
func wrapStepUsage(wf *wfv1.Workflow) {
	walkTemplates(wf, func(t *wfv1.Template) {
		c := usageContainer(t)
		if c == nil || len(c.Command) == 0 || (c.TerminationMessagePath != "" && c.TerminationMessagePath != stepusage.TerminationPath) {
			return
		}
		mounted := slices.ContainsFunc(c.VolumeMounts, func(m corev1.VolumeMount) bool {
			return m.Name == SourceVolumeName && m.MountPath == usageBinMountPath && m.ReadOnly
		})
		if !mounted {
			return
		}
		if len(usageOriginalCommand(c.Command)) == len(c.Command) {
			c.Command = append([]string{usageBinPath, usageCommand, "--"}, c.Command...)
		}
	})
}

func projectStepUsage(workflow *wfv1.Workflow, node wfv1.NodeStatus, pod *corev1.Pod) stepusage.Usage {
	if !stepPodBound(workflow, node, pod) {
		return stepusage.Usage{}
	}
	// Reuse the strict current main/container-ID/attempt checks, independently of
	// diagnostics classification. Never consume a prior restart's LastTerminationState.
	if d := projectStepDiagnostic(workflow, node, pod, node.FinishedAt.Time); !d.Available {
		return stepusage.Usage{}
	}
	for _, c := range pod.Spec.Containers {
		if c.Name != "main" {
			continue
		}
		// Argo prepends its emissary entrypoint, so find the exact wrapper suffix.
		command := append(slices.Clone(c.Command), c.Args...)
		wrapped := false
		for i := 0; i+3 < len(command); i++ {
			if slices.Equal(command[i:i+3], []string{usageBinPath, usageCommand, "--"}) {
				wrapped = true
				break
			}
		}
		if !wrapped || (c.TerminationMessagePath != "" && c.TerminationMessagePath != stepusage.TerminationPath) {
			return stepusage.Usage{}
		}
	}
	for _, s := range pod.Status.ContainerStatuses {
		if s.Name == "main" && s.RestartCount == 0 && s.State.Terminated != nil {
			return stepusage.Decode(s.State.Terminated.Message)
		}
	}
	return stepusage.Usage{}
}

// Credential classification also runs on admitted workflows after wrapping.
func usageOriginalCommand(command []string) []string {
	if len(command) > 3 && slices.Equal(command[:3], []string{usageBinPath, usageCommand, "--"}) {
		return command[3:]
	}
	return command
}

func workflowHasStepUsage(wf *wfv1.Workflow) bool {
	found := false
	walkTemplates(wf, func(t *wfv1.Template) {
		if c := usageContainer(t); c != nil && len(usageOriginalCommand(c.Command)) != len(c.Command) {
			found = true
		}
	})
	return found
}
