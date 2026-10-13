package argojob

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/oberthci/oberth/internal/vmrunner"
)

// ErrTrustedLeafNotFound means the trusted-tests leaf's pod was not found.
var ErrTrustedLeafNotFound = errors.New("argojob: trusted-tests leaf pod not found")

// ErrTrustedLeafNotTerminal means the trusted-tests leaf has not completed.
var ErrTrustedLeafNotTerminal = errors.New("argojob: trusted-tests leaf is not terminal")

// ReadTrustedLeafVerdict reads the kubelet termination record of the
// trusted-tests leaf from the Kubernetes API. The record comes from the
// kubelet, never from repository-written output.
//
// The function finds the pod by label selector matching the workflow name
// and the trusted-tests template name, reads its container status, and
// returns a KubeletRecord with the verdict-relevant fields.
func ReadTrustedLeafVerdict(ctx context.Context, client kubernetes.Interface, namespace, workflowName string) (vmrunner.KubeletRecord, error) {
	if client == nil {
		return vmrunner.KubeletRecord{}, errors.New("argojob: Kubernetes client is required for trusted leaf verdict")
	}

	// The Argo controller names pods with the workflow name as prefix and
	// adds labels for the template name.
	selector := fmt.Sprintf("workflows.argoproj.io/workflow=%s", workflowName)

	pods, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
		Limit:         64,
	})
	if err != nil {
		return vmrunner.KubeletRecord{}, fmt.Errorf("argojob: list pods for trusted leaf: %w", err)
	}

	var trustedPod *corev1.Pod
	for i := range pods.Items {
		pod := &pods.Items[i]
		// The Argo controller sets this annotation on each pod with the
		// template name that created it.
		templateName, ok := pod.Annotations["workflows.argoproj.io/template"]
		if !ok {
			// Try the label as well.
			templateName = pod.Labels["workflows.argoproj.io/template"]
		}
		if templateName == vmrunner.BurnName {
			trustedPod = pod
			break
		}
	}
	if trustedPod == nil {
		return vmrunner.KubeletRecord{}, ErrTrustedLeafNotFound
	}

	return extractKubeletRecord(trustedPod)
}

// extractKubeletRecord reads the termination record from a pod's container
// status. Only the "main" container is relevant (Argo convention).
func extractKubeletRecord(pod *corev1.Pod) (vmrunner.KubeletRecord, error) {
	record := vmrunner.KubeletRecord{
		PodUID: string(pod.UID),
	}

	// Find the main container's status. Argo names its main container "main".
	var mainStatus *corev1.ContainerStatus
	for i := range pod.Status.ContainerStatuses {
		cs := &pod.Status.ContainerStatuses[i]
		if cs.Name == "main" {
			mainStatus = cs
			break
		}
	}
	if mainStatus == nil {
		return record, ErrTrustedLeafNotTerminal
	}

	record.ContainerID = mainStatus.ContainerID
	record.ImageID = mainStatus.ImageID
	record.RestartCount = mainStatus.RestartCount

	if mainStatus.State.Terminated != nil {
		record.ExitCode = mainStatus.State.Terminated.ExitCode
		record.Message = mainStatus.State.Terminated.Message
		record.FinishedAt = mainStatus.State.Terminated.FinishedAt.Time
	} else if mainStatus.LastTerminationState.Terminated != nil {
		// Container was restarted; use the last termination state.
		record.ExitCode = mainStatus.LastTerminationState.Terminated.ExitCode
		record.Message = mainStatus.LastTerminationState.Terminated.Message
		record.FinishedAt = mainStatus.LastTerminationState.Terminated.FinishedAt.Time
	} else {
		return record, ErrTrustedLeafNotTerminal
	}

	return record, nil
}

// BuildTrustedReceipt constructs a receipt from the contract, requirement,
// and kubelet record. The verdict is evaluated according to the closed
// negative receipt rules. The harnessImageRef is the digest-pinned harness
// image the server configured; the kubelet's imageID is cross-checked
// against its digest to detect wrong-image substitution.
func BuildTrustedReceipt(
	runID string,
	contract vmrunner.Contract,
	suiteIdentity string,
	contractDigest string,
	record vmrunner.KubeletRecord,
	harnessImageRef string,
	startedAt time.Time,
) vmrunner.TrustedTestReceipt {
	// Extract guest digest from the contract's guest image.
	guestDigest := contract.GuestImage
	if idx := strings.LastIndex(guestDigest, "@"); idx >= 0 {
		guestDigest = guestDigest[idx+1:]
	}

	// The expected digest for wrong-digest cross-check is the harness image
	// digest, not the guest digest. The harness image is what the kubelet
	// actually ran; the guest image is an argument to it.
	verdict, reason := vmrunner.EvaluateKubeletVerdict(record, harnessImageRef, contract.Timeout, startedAt)

	return vmrunner.TrustedTestReceipt{
		RunID:           runID,
		Profile:         contract.Profile,
		CandidateSHA:    "", // Set by caller from run.TestedSHA
		SuiteIdentity:   suiteIdentity,
		GuestDigest:     guestDigest,
		ContractDigest:  contractDigest,
		Verdict:         verdict,
		PodUID:          record.PodUID,
		ContainerID:     record.ContainerID,
		ExitCode:        record.ExitCode,
		RestartCount:    record.RestartCount,
		TerminationHash: vmrunner.TerminationMessageHash(record.Message),
		RefusalReason:   reason,
	}
}
