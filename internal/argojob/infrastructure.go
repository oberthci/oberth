package argojob

import (
	"regexp"
	"strings"
	"unicode/utf8"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"

	"github.com/oberthci/oberth/internal/runprogress"
)

// Argo copies PodStatus.Message verbatim on eviction. This survives Pod GC.
// OOMKilled alone does not distinguish a step's memory limit from node pressure;
// only a kubelet eviction/pressure message establishes an infrastructure cause.
var kubeletEvictionMessage = regexp.MustCompile(`^(Usage of EmptyDir volume "[^"]+" exceeds the limit "[^"]+"\.|The node was low on resource: (ephemeral-storage|memory)\.|Pod ephemeral local storage usage exceeds the total limit of containers|Container [a-z0-9-]+ exceeded its local ephemeral storage limit)`)

func nodeInfrastructureReason(node wfv1.NodeStatus) string {
	if node.Type != wfv1.NodeTypePod || (node.Phase != wfv1.NodeFailed && node.Phase != wfv1.NodeError) {
		return ""
	}
	message := strings.TrimSpace(node.Message)
	if kubeletEvictionMessage.MatchString(message) {
		return boundedKubeletMessage("Evicted: " + message)
	}
	return ""
}

func podInfrastructureReason(workflow *wfv1.Workflow, node wfv1.NodeStatus, pod *corev1.Pod) string {
	// Eviction may precede main startup or leave running container states, so
	// it needs the owned Pod binding, not a complete container diagnostic.
	if !stepPodBound(workflow, node, pod) || pod.Status.Phase != corev1.PodFailed {
		return ""
	}
	message := strings.TrimSpace(pod.Status.Message)
	if pod.Status.Reason == "Evicted" || kubeletEvictionMessage.MatchString(message) {
		if message == "" {
			return "Evicted"
		}
		return boundedKubeletMessage("Evicted: " + message)
	}
	return ""
}

func boundedKubeletMessage(message string) string {
	message = strings.ToValidUTF8(message, "")
	if len(message) > 4096 {
		message = message[:4096]
		for !utf8.ValidString(message) {
			message = message[:len(message)-1]
		}
	}
	return message
}

func (completion *Completion) classifyInfrastructure() {
	if completion.Succeeded {
		return
	}
	reason := ""
	failedBurn, failedStep := "", ""
	for _, step := range completion.Steps {
		if step.Status == runprogress.StepPassed || step.Status == runprogress.StepSkipped || !terminal(step.Status) {
			continue
		}
		// A separate test failure must still open a CI-red issue and must not
		// become eligible for infrastructure retry.
		if step.AttemptAmbiguous || step.InfrastructureReason == "" {
			failedBurn, failedStep = step.Burn, step.Step
			continue
		}
		if reason == "" {
			reason = step.InfrastructureReason
		}
	}
	if failedStep != "" {
		if reason != "" {
			completion.FailedBurn, completion.FailedStep = failedBurn, failedStep
		}
		return
	}
	if reason != "" {
		completion.Infrastructure = true
		completion.Reason = reason
	}
}
