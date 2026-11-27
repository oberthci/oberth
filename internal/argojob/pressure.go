package argojob

import (
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"strings"

	"github.com/oberthci/oberth/internal/stepmodel"
	core "k8s.io/api/core/v1"
)

// Pressure describes an affected assigned Pod, never spare capacity. A healthy
// executing Pod does not inherit its Node's pressure.
func pressureDiagnosis(d diagnosis, native wf.NodeStatus, pod *core.Pod, nodes []*core.Node) diagnosis {
	if pod == nil || pod.Spec.NodeName == "" || pod.Status.Reason != "Evicted" {
		return d
	}
	for _, node := range nodes {
		if node.Name != pod.Spec.NodeName || retainedUID(string(node.UID)) == "" {
			continue
		}
		for _, condition := range node.Status.Conditions {
			if !pressureAtFailure(native, condition) {
				continue
			}
			if reason := relevantPressure(condition.Type, pod.Status.Message); reason != "" {
				d.reason = reason
				d.class = stepmodel.FailureInfrastructure
				d.fact = &stepmodel.NativeDiagnostic{Cause: pressureCause(reason), Source: "node", NativeUID: string(node.UID), TransitionAt: new(condition.LastTransitionTime.Time)}
				return d
			}
		}
	}
	return d
}

func pressureAtFailure(native wf.NodeStatus, condition core.NodeCondition) bool {
	return condition.Status == core.ConditionTrue && !condition.LastTransitionTime.IsZero() &&
		!native.FinishedAt.IsZero() && !condition.LastTransitionTime.After(native.FinishedAt.Time)
}

func relevantPressure(condition core.NodeConditionType, message string) string {
	text := strings.ToLower(message[:min(len(message), 16<<10)])
	switch condition {
	case core.NodeDiskPressure:
		if strings.Contains(text, "ephemeral-storage") || strings.Contains(text, "disk") {
			return reasonDiskPressure
		}
	case core.NodeMemoryPressure:
		if strings.Contains(text, "memory") {
			return reasonMemoryPressure
		}
	case core.NodePIDPressure:
		if strings.Contains(text, "pid") || strings.Contains(text, "process") {
			return reasonPIDPressure
		}
	}
	return ""
}
