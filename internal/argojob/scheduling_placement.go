package argojob

import (
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

// Preserve scheduler reason phrases, without copying unrelated free-form status
// text. Resource names use Kubernetes' qualified-name grammar; no events or
// additional API reads are needed. At most four phrases are exposed per Pod.
func schedulerPlacementReason(pod corev1.Pod) string {
	if pod.Status.Phase != corev1.PodPending {
		return ""
	}
	reasons := map[string]bool{}
	for _, condition := range pod.Status.Conditions {
		if condition.Type != corev1.PodScheduled || condition.Status != corev1.ConditionFalse || condition.Reason != "Unschedulable" {
			continue
		}
		fields := strings.Fields(condition.Message)
		for i := 0; i+1 < len(fields); i++ {
			if fields[i] != "Insufficient" {
				continue
			}
			name := strings.TrimRight(fields[i+1], ".,;:")
			if strings.Contains(name, "/") && len(validation.IsQualifiedName(name)) == 0 {
				reasons["Insufficient "+name] = true
			}
		}
		for _, phrase := range []string{
			"node(s) didn't match pod anti-affinity rules",
			"node(s) didn't satisfy existing pods anti-affinity rules",
			"node(s) didn't match pod affinity rules",
			"node(s) didn't match Pod's node affinity/selector",
		} {
			if hasSchedulingPhrase(condition.Message, strings.Fields(phrase)...) {
				reasons[phrase] = true
			}
		}
	}
	names := make([]string, 0, len(reasons))
	for reason := range reasons {
		names = append(names, reason)
	}
	sort.Strings(names)
	return strings.Join(names[:min(4, len(names))], "; ")
}
