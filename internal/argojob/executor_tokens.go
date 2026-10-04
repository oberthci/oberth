package argojob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

const executorTokenWaiter = "oberth-executor-token"

// #nosec G101 -- fixed runtime path/command, not credential material.
const executorTokenVolume = "exec-sa-token"

// #nosec G101 -- fixed runtime path/command, not credential material.
const executorTokenDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// #nosec G101 -- fixed runtime path/command, not credential material.
const executorTokenWaitCommand = "until test -f /var/run/secrets/kubernetes.io/serviceaccount/token; do sleep 1; done"

// Argo adds a Secret-backed executor volume before applying the server-owned
// pod patch. Replace that volume with tmpfs and wait for a Pod-bound TokenRequest
// delivered by Oberth over exec. No token is present in a Kubernetes object.
func projectExecutorTokens(workflow *wfv1.Workflow, config Config) error {
	var result error
	walkTemplates(workflow, func(t *wfv1.Template) {
		if result != nil || (t.Container == nil && t.Script == nil && t.ContainerSet == nil && t.Resource == nil && t.Data == nil) {
			return
		}
		patch := map[string]any{}
		if t.PodSpecPatch != "" {
			if err := json.Unmarshal([]byte(t.PodSpecPatch), &patch); err != nil {
				result = err
				return
			}
		}
		volumes, _ := patch["volumes"].([]any)
		patch["volumes"] = append(volumes, map[string]any{"name": executorTokenVolume, "secret": nil, "emptyDir": map[string]any{"medium": "Memory", "sizeLimit": "1Mi"}})
		waiter := corev1.Container{Name: executorTokenWaiter, Image: config.SourceSeedImage, Command: []string{"sh", "-c", executorTokenWaitCommand}, SecurityContext: nonrootContainerSecurity(0), VolumeMounts: []corev1.VolumeMount{{Name: executorTokenVolume, MountPath: executorTokenDir}}}
		initializers, _ := patch["initContainers"].([]any)
		if len(initializers) == 0 {
			initializers = []any{map[string]any{"name": "init"}}
		}
		merged := make([]any, 0, len(initializers)+1)
		order := []map[string]string{}
		inserted := false
		for _, item := range initializers {
			if value, ok := item.(map[string]any); ok {
				if name, ok := value["name"].(string); ok {
					if name == "init" {
						merged = append(merged, waiter)
						order = append(order, map[string]string{"name": executorTokenWaiter})
						inserted = true
					}
					order = append(order, map[string]string{"name": name})
				}
			}
			merged = append(merged, item)
		}
		if !inserted {
			result = errors.New("executor initializer missing from server patch")
			return
		}
		patch["initContainers"] = merged
		patch["$setElementOrder/initContainers"] = order

		encoded, err := json.Marshal(patch)
		if err != nil {
			result = err
			return
		}
		t.PodSpecPatch = string(encoded)
		if t.SecurityContext != nil && t.SecurityContext.RunAsNonRoot != nil && *t.SecurityContext.RunAsNonRoot {
			result = argoworkflow.ValidateNonrootTemplateSize(t)
		}
	})
	return result
}

func (controller *Controller) deliverExecutorTokens(ctx context.Context, wf *wfv1.Workflow, delivered map[types.UID]bool) error {
	if controller.kube == nil || controller.seeder == nil {
		return nil
	}
	pods, err := controller.kube.CoreV1().Pods(wf.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "workflows.argoproj.io/workflow=" + wf.Name})
	if err != nil {
		return err
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if delivered[pod.UID] || pod.DeletionTimestamp != nil {
			continue
		}
		owned := false
		for _, ref := range pod.OwnerReferences {
			if ref.UID == wf.UID && ref.Kind == "Workflow" && ref.Controller != nil && *ref.Controller {
				owned = true
			}
		}
		if !owned {
			continue
		}
		running := false
		for _, status := range pod.Status.InitContainerStatuses {
			if status.Name == executorTokenWaiter && status.State.Running != nil {
				running = true
			}
		}
		if !running {
			continue
		}
		if err := validateExecutorTokenTarget(pod, controller.config.SourceSeedImage); err != nil {
			return err
		}
		// Bound tokens die with the workflow-owned identity Pod. The deadline covers the workflow,
		// including queue/startup slack, without needing a long-lived token Secret.
		seconds := int64(controller.config.WorkflowTimeout/time.Second) + 600
		if wf.Spec.ActiveDeadlineSeconds != nil {
			seconds = *wf.Spec.ActiveDeadlineSeconds + 600
		}
		if seconds < 600 {
			seconds = 600
		}
		holder, err := controller.executorIdentityPod(ctx, wf)
		if err != nil {
			return err
		}
		token, err := controller.kube.CoreV1().ServiceAccounts(wf.Namespace).CreateToken(ctx, controller.config.ExecutorServiceAccount, &authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{ExpirationSeconds: &seconds, BoundObjectRef: &authenticationv1.BoundObjectReference{Kind: "Pod", APIVersion: "v1", Name: holder.Name, UID: holder.UID}}}, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("request executor identity: %w", err)
		}
		if token.Status.ExpirationTimestamp.Time.Before(time.Now().Add(time.Duration(seconds-60) * time.Second)) {
			return errors.New("executor token lifetime is shorter than the workflow deadline")
		}
		if token.Status.Token == "" {
			return errors.New("empty executor token response")
		}
		ca, err := controller.kube.CoreV1().ConfigMaps(wf.Namespace).Get(ctx, "kube-root-ca.crt", metav1.GetOptions{})
		if err != nil {
			return err
		}
		if ca.Data["ca.crt"] == "" {
			return errors.New("executor API trust anchor is empty")
		}
		payload := []byte(token.Status.Token + "\n" + wf.Namespace + "\n" + ca.Data["ca.crt"])
		// The marker is renamed last, after the public metadata is complete.
		command := []string{"sh", "-c", `umask 077; cd /var/run/secrets/kubernetes.io/serviceaccount || exit 1; IFS= read -r token; IFS= read -r namespace; cat > ca.crt; printf '%s' "$namespace" > namespace; printf '%s' "$token" > .token; chmod 0444 ca.crt namespace .token; mv .token token`}
		deliveryCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		err = controller.seeder.exec(deliveryCtx, wf.Namespace, pod.Name, executorTokenWaiter, command, bytes.NewReader(payload), io.Discard, io.Discard)
		cancel()
		clear(payload)
		if err != nil {
			return errors.New("deliver executor identity failed")
		}
		delivered[pod.UID] = true
	}
	return nil
}

func validateExecutorTokenTarget(pod *corev1.Pod, image string) error {
	validVolume := false
	for _, v := range pod.Spec.Volumes {
		if v.Name == executorTokenVolume {
			validVolume = v.EmptyDir != nil && v.EmptyDir.Medium == corev1.StorageMediumMemory && v.Secret == nil
		}
	}
	if !validVolume {
		return errors.New("executor token target is not memory-backed")
	}
	for _, container := range pod.Spec.EphemeralContainers {
		for _, mount := range container.VolumeMounts {
			if mount.Name == executorTokenVolume {
				return errors.New("ephemeral container can access executor identity")
			}
		}
	}
	found := false
	for _, container := range append(append([]corev1.Container{}, pod.Spec.InitContainers...), pod.Spec.Containers...) {
		if container.Name == executorTokenWaiter {
			if container.Image != image || len(container.Command) != 3 || container.Command[0] != "sh" || container.Command[1] != "-c" || len(container.Args) != 0 || container.Command[2] != executorTokenWaitCommand {
				return errors.New("executor identity waiter was modified")
			}
			found = true
		}
		for _, mount := range container.VolumeMounts {
			if mount.Name == executorTokenVolume && container.Name != executorTokenWaiter && container.Name != "init" && container.Name != "wait" {
				return errors.New("pipeline container can access executor identity")
			}
		}
	}
	if !found {
		return errors.New("executor identity waiter missing")
	}
	return nil
}

// Kubernetes requires a bound Pod to use the token's ServiceAccount. A separate
// workflow-owned Pod preserves executor/pipeline separation. Its scheduling
// gate is never removed: it represents credential lifetime and runs no code.
// Deleting the workflow garbage-collects this Pod and invalidates its tokens.
func (controller *Controller) executorIdentityPod(ctx context.Context, wf *wfv1.Workflow) (*corev1.Pod, error) {
	name := executorIdentityPodName(wf.UID)
	desired := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: wf.Namespace, Labels: map[string]string{"oberth.ci/role": "executor-identity"}, OwnerReferences: []metav1.OwnerReference{{APIVersion: "argoproj.io/v1alpha1", Kind: "Workflow", Name: wf.Name, UID: wf.UID, Controller: ptr.To(true)}}},
		Spec:       corev1.PodSpec{ServiceAccountName: controller.config.ExecutorServiceAccount, AutomountServiceAccountToken: ptr.To(false), RestartPolicy: corev1.RestartPolicyNever, SchedulingGates: []corev1.PodSchedulingGate{{Name: "oberth.ci/executor-identity"}}, Containers: []corev1.Container{{Name: "identity", Image: controller.config.SourceSeedImage, Command: []string{"/bin/false"}, SecurityContext: nonrootContainerSecurity(65534), Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1m"), corev1.ResourceMemory: resource.MustParse("1Mi")}, Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("8Mi")}}}}},
	}
	pods := controller.kube.CoreV1().Pods(wf.Namespace)
	pod, err := pods.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		pod, err = pods.Create(ctx, desired, metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(err) {
			pod, err = pods.Get(ctx, name, metav1.GetOptions{})
		}
	}
	if err != nil {
		return nil, fmt.Errorf("create executor identity lifetime: %w", err)
	}
	if pod.DeletionTimestamp != nil || pod.UID == "" || pod.Spec.ServiceAccountName != controller.config.ExecutorServiceAccount || pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken || len(pod.Spec.SchedulingGates) != 1 || pod.Spec.SchedulingGates[0].Name != "oberth.ci/executor-identity" || len(pod.Spec.Volumes) != 0 || len(pod.Spec.InitContainers) != 0 || len(pod.Spec.EphemeralContainers) != 0 || len(pod.Spec.Containers) != 1 || pod.Spec.Containers[0].Name != "identity" || pod.Spec.Containers[0].Image != controller.config.SourceSeedImage || len(pod.Spec.Containers[0].Command) != 1 || pod.Spec.Containers[0].Command[0] != "/bin/false" || len(pod.OwnerReferences) != 1 || pod.OwnerReferences[0].UID != wf.UID || pod.OwnerReferences[0].Kind != "Workflow" || pod.OwnerReferences[0].Controller == nil || !*pod.OwnerReferences[0].Controller {
		return nil, errors.New("executor identity lifetime Pod has unexpected ownership or configuration")
	}
	return pod, nil
}

func executorIdentityPodName(uid types.UID) string {
	digest := sha256.Sum256([]byte(uid))
	return fmt.Sprintf("oberth-executor-%x", digest[:12])
}

func (controller *Controller) retireExecutorIdentity(ctx context.Context, wf *wfv1.Workflow) error {
	if controller.kube == nil || wf.UID == "" {
		return nil
	}
	pods := controller.kube.CoreV1().Pods(wf.Namespace)
	pod, err := pods.Get(ctx, executorIdentityPodName(wf.UID), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(pod.OwnerReferences) != 1 || pod.OwnerReferences[0].UID != wf.UID {
		return errors.New("executor identity owner changed")
	}
	err = pods.Delete(ctx, pod.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &pod.UID}, GracePeriodSeconds: ptr.To(int64(0))})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}
