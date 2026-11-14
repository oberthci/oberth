package argojob

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

const executorTokenWaiter = "oberth-executor-token"

// Give wait a head start reading Argo's shared template before main and sidecars; this is a margin, not a race proof.
const executorWaitStartDelaySeconds int64 = 1

// #nosec G101 -- fixed runtime path/command, not credential material.
const executorTokenVolume = argoworkflow.ExecutorIdentityVolumeName

// #nosec G101 -- fixed runtime path/command, not credential material.
const executorTokenDir = argoworkflow.ExecutorIdentityMountPath

// executorKubeconfig is the only fixed name the executor identity has inside
// the tmpfs. Argo's wait container reads outputs.parameters valueFrom.path and
// the script result with plain os.ReadFile in its own mount namespace, so a
// symlink a step plants is resolved against wait's filesystem (#1564). The
// in-cluster default path /var/run/secrets/kubernetes.io/serviceaccount/token
// therefore stays absent: the token lives under a 128-bit random directory
// named only by this kubeconfig, which client-go reads through KUBECONFIG. A
// symlink target is a string fixed before wait reads it, and the name is
// created at delivery inside a mount namespace no step can enter.
// #nosec G101 -- fixed runtime path/command, not credential material.
const executorKubeconfig = executorTokenDir + "/kubeconfig"

// #nosec G101 -- fixed runtime path/command, not credential material.
const executorTokenWaitCommand = `trap 'printf "%s\n" "executor token wait terminated" >&2; exit 143' TERM
trap 'printf "%s\n" "executor token wait interrupted" >&2; exit 130' INT
printf '%s\n' 'waiting for executor token from Oberth' >&2
waited_seconds=0
until test -f /var/run/secrets/kubernetes.io/serviceaccount/kubeconfig; do
  if ! sleep 1; then
    printf '%s\n' 'executor token wait failed' >&2
    exit 1
  fi
  waited_seconds=$((waited_seconds + 1))
  if [ "$((waited_seconds % 30))" -eq 0 ]; then
    printf 'still waiting for executor token from Oberth after %ss\n' "$waited_seconds" >&2
  fi
done
printf '%s\n' 'executor token ready' >&2`

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
		waiter := executorWaiterContainer(config.SourceSeedImage, helperResources(workflow, t))
		initializers, _ := patch["initContainers"].([]any)
		if len(initializers) == 0 {
			initializers = []any{map[string]any{"name": "init"}}
		}
		// Argo installs its runtime/template before authored initContainers,
		// and adds their runtime mount before strategic merge. They never run
		// emissary and must not alter the template wait will load with authority.
		for _, authored := range t.InitContainers {
			var initializer map[string]any
			for _, item := range initializers {
				if value, ok := item.(map[string]any); ok && value["name"] == authored.Name {
					initializer = value
					break
				}
			}
			if initializer == nil {
				initializer = map[string]any{"name": authored.Name}
				initializers = append(initializers, initializer)
			}
			mounts, _ := initializer["volumeMounts"].([]any)
			mount := map[string]any{"name": "var-run-argo", "mountPath": "/var/run/argo", "readOnly": true}
			replaced := false
			for i, item := range mounts {
				if value, ok := item.(map[string]any); ok && value["mountPath"] == "/var/run/argo" {
					mounts[i] = mount
					replaced = true
				}
			}
			if !replaced {
				mounts = append(mounts, mount)
			}
			initializer["volumeMounts"] = mounts
		}
		merged := make([]any, 0, len(initializers)+1)
		order := []map[string]string{}
		inserted := false
		for _, item := range initializers {
			if value, ok := item.(map[string]any); ok {
				if name, ok := value["name"].(string); ok {
					if name == "init" {
						setExecutorEnvironment(value)
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
		// Match Argo: resource/data executors normally run only main. A
		// resource template gains wait only when it archives logs.
		hasWait := (t.Resource == nil && t.Data == nil) || (t.Resource != nil && t.SaveLogsAsArtifact())
		containers, _ := patch["containers"].([]any)
		// Every Argo executor container that mounts the identity finds it
		// through the kubeconfig; the in-cluster default token path is empty.
		executors := []string{}
		if hasWait {
			executors = append(executors, "wait")
		}
		if t.Resource != nil || t.Data != nil {
			executors = append(executors, "main")
		}
		for _, name := range executors {
			found := false
			for _, item := range containers {
				if value, ok := item.(map[string]any); ok && value["name"] == name {
					setExecutorEnvironment(value)
					found = true
				}
			}
			if !found {
				value := map[string]any{"name": name}
				setExecutorEnvironment(value)
				containers = append(containers, value)
			}
		}
		if len(containers) != 0 {
			patch["containers"] = containers
		}

		patch["initContainers"] = merged
		patch["$setElementOrder/initContainers"] = order

		encoded, err := json.Marshal(patch)
		if err != nil {
			result = err
			return
		}
		t.PodSpecPatch = string(encoded)
	})
	return result
}

// executorTokenDirectory names the directory the token is delivered under. It
// exists only in the Pod's tmpfs and the kubeconfig there, never in a
// Kubernetes object, so no step can name it before wait reads its outputs.
func executorTokenDirectory() string {
	var name [16]byte
	_, _ = rand.Read(name[:]) // crypto/rand.Read never returns an error.
	return hex.EncodeToString(name[:])
}

// executorEnvironment is what every Argo executor container that holds the
// identity gets through the server patch. KUBECONFIG names the delivered
// kubeconfig. The progress ticks are zero because Argo v4.0.8's wait runs
// monitorProgress (executor.go Wait) on the command context, so it keeps
// patching WorkflowTaskResults while wait reads a step's outputs; each patch
// can re-read the token file, briefly opening it on a low descriptor that a
// step's /proc/self/fd/N symlink would resolve to. A zero tick disables the
// monitor (Wait starts it only when both ticks are nonzero), leaving no API
// call between InitializeOutput and ReportOutputs. Self-reported step
// progress (N/M) is therefore not shown. Strategic merge replaces only the
// first duplicate env entry; validateExecutorTokenTarget checks the live Pod
// before delivery so controller configuration or Argo drift cannot win.
// On every Argo bump, re-audit all go statements in workflow/executor/executor.go
// for API calls during output collection and re-run the #1564 hostile tests.
var executorEnvironment = [][2]string{
	{"KUBECONFIG", executorKubeconfig},
	{"ARGO_PROGRESS_PATCH_TICK_DURATION", "0"},
	{"ARGO_PROGRESS_FILE_TICK_DURATION", "0"},
}

func setExecutorEnvironment(container map[string]any) {
	env, _ := container["env"].([]any)
	for _, pair := range executorEnvironment {
		found := false
		for _, item := range env {
			if value, ok := item.(map[string]any); ok && value["name"] == pair[0] {
				value["value"] = pair[1]
				delete(value, "valueFrom")
				found = true
			}
		}
		if !found {
			env = append(env, map[string]any{"name": pair[0], "value": pair[1]})
		}
	}
	container["env"] = env
	if container["name"] == "wait" {
		container["lifecycle"] = map[string]any{"postStart": map[string]any{"sleep": map[string]any{"seconds": executorWaitStartDelaySeconds}}}
	}
}

func (controller *Controller) deliverExecutorTokensObserved(ctx context.Context, wf *wfv1.Workflow, delivered map[types.UID]bool, unavailable func()) error {
	if wf.Status.Phase.Completed() || controller.kube == nil || controller.seeder == nil {
		return nil
	}
	capability, err := controller.boundCapability(ctx, wf, 0)
	if err != nil {
		return identityFailure("operator_capability", err)
	}
	pods, err := controller.kube.CoreV1().Pods(wf.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "workflows.argoproj.io/workflow=" + wf.Name})
	if err != nil {
		if ctx.Err() == nil && retryExecutorIdentityRead(err) {
			if unavailable != nil {
				unavailable()
			}
			return nil
		}
		return identityFailure("pod_list", err)
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
		// Only a resource Pod's main container can mount the executor token.
		// Its node mapping must settle before selecting the operator identity;
		// ordinary steps can receive their executor token immediately.
		if nodeID := pod.Annotations["workflows.argoproj.io/node-id"]; nodeID != "" && mainMountsExecutorToken(pod) {
			if _, ok := wf.Status.Nodes[nodeID]; !ok {
				continue
			}
		}
		resourceAccount := ""
		if capability != nil {
			resourceAccount = capability.ServiceAccount
		}
		launcher := resourcePodTemplate(wf, pod, resourceAccount)
		if err := validateExecutorTokenTarget(pod, controller.config.SourceSeedImage, launcher); err != nil {
			return identityFailure("target_validation", err)
		}
		serviceAccount := controller.config.ExecutorServiceAccount
		if launcher && capability != nil {
			serviceAccount = capability.ServiceAccount
		}
		// Bound tokens die with the workflow-owned identity Pod. The deadline covers the workflow,
		// including queue/startup slack, without needing a long-lived token Secret.
		seconds := int64((12*time.Hour)/time.Second) + 600
		if wf.Spec.ActiveDeadlineSeconds != nil {
			seconds = *wf.Spec.ActiveDeadlineSeconds + 600
		}
		if seconds < 600 {
			seconds = 600
		}
		holder, err := controller.executorIdentityPod(ctx, wf, serviceAccount)
		if err != nil {
			if ctx.Err() == nil && retryExecutorIdentityRead(err) {
				if unavailable != nil {
					unavailable()
				}
				continue
			}
			return identityFailure("identity_pod", err)
		}
		ca, err := controller.kube.CoreV1().ConfigMaps(wf.Namespace).Get(ctx, "kube-root-ca.crt", metav1.GetOptions{})
		if err != nil {
			if ctx.Err() == nil && retryExecutorIdentityRead(err) {
				if unavailable != nil {
					unavailable()
				}
				continue
			}
			return identityFailure("trust_anchor", err)
		}
		if ca.Data["ca.crt"] == "" {
			return identityFailure("trust_anchor_response", errors.New("executor API trust anchor is empty"))
		}
		token, err := controller.requestExecutorIdentity(ctx, wf.Namespace, serviceAccount, &authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{ExpirationSeconds: &seconds, BoundObjectRef: &authenticationv1.BoundObjectReference{Kind: "Pod", APIVersion: "v1", Name: holder.Name, UID: holder.UID}}}, wf.Name)
		if err != nil {
			return identityFailure("token_request", err)
		}
		if token.Status.ExpirationTimestamp.Time.Before(time.Now().Add(time.Duration(seconds-60) * time.Second)) {
			return identityFailure("token_response", errors.New("executor token lifetime is shorter than the workflow deadline"))
		}
		if token.Status.Token == "" {
			return identityFailure("token_response", errors.New("empty executor token response"))
		}
		payload := []byte(token.Status.Token + "\n" + wf.Namespace + "\n" + base64.StdEncoding.EncodeToString([]byte(ca.Data["ca.crt"])) + "\n" + executorTokenDirectory() + "\n")
		err = controller.execExecutorIdentity(ctx, wf, pod, launcher, payload)
		clear(payload)
		if errors.Is(err, errExecutorIdentityTargetGone) {
			continue
		}
		if err != nil {
			return err
		}
		delivered[pod.UID] = true
	}
	return nil
}

func mainMountsExecutorToken(pod *corev1.Pod) bool {
	for _, container := range pod.Spec.Containers {
		if container.Name == "main" {
			for _, mount := range container.VolumeMounts {
				if mount.Name == executorTokenVolume {
					return true
				}
			}
		}
	}
	return false
}

// A controller-owned Pod node resolves to a named, admitted resource template.
// A Pod annotation alone cannot select the operator identity.
func resourcePodTemplate(wf *wfv1.Workflow, pod *corev1.Pod, operatorSA string) bool {
	if operatorSA == "" {
		return false
	}
	node, ok := wf.Status.Nodes[pod.Annotations["workflows.argoproj.io/node-id"]]
	if !ok || node.TemplateName == "" {
		return false
	}
	for i := range wf.Spec.Templates {
		t := &wf.Spec.Templates[i]
		if t.Name == node.TemplateName && t.Resource != nil && t.Executor != nil && t.Executor.ServiceAccountName == operatorSA {
			return true
		}
	}
	return false
}

func validateExecutorTokenTarget(pod *corev1.Pod, image string, resourcePod bool) error {
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
		if strings.HasPrefix(container.Name, "oberth-") {
			return errors.New("ephemeral container uses a server-owned name")
		}
		for _, mount := range container.VolumeMounts {
			if mount.Name == executorTokenVolume || argoScratchVolume(mount.Name) {
				return errors.New("ephemeral container can access executor identity")
			}
		}
	}
	found := false
	serverNames := map[string]bool{}
	for position, container := range append(append([]corev1.Container{}, pod.Spec.InitContainers...), pod.Spec.Containers...) {
		initializer := position < len(pod.Spec.InitContainers)
		if strings.HasPrefix(container.Name, "oberth-") {
			if serverNames[container.Name] || !validServerContainer(container, image, initializer) {
				return errors.New("server-added container was modified or duplicated")
			}
			serverNames[container.Name] = true
		}
		for _, device := range container.VolumeDevices {
			if argoScratchVolume(device.Name) {
				return errors.New("container can access Argo scratch device")
			}
		}
		if container.Name == executorTokenWaiter {
			found = true
		}
		if !initializer && container.Name == "wait" && container.Image == argoworkflow.ExecutorImage {
			expected := &corev1.Lifecycle{PostStart: &corev1.LifecycleHandler{Sleep: &corev1.SleepAction{Seconds: executorWaitStartDelaySeconds}}}
			// Kubelet starts regular containers in list order, so the margin needs wait first.
			if position != len(pod.Spec.InitContainers) || !apiequality.Semantic.DeepEqual(container.Lifecycle, expected) {
				return errors.New("executor wait start hook differs from the server patch")
			}
		}
		scratchCounts := map[string]int{}
		for _, mount := range container.VolumeMounts {
			scratchCounts[mount.Name]++
			if argoScratchVolume(mount.Name) && (scratchCounts[mount.Name] != 1 || !validArgoScratchMount(container, mount, initializer, resourcePod, serverNames[container.Name])) {
				return errors.New("pipeline helper can access Argo executor scratch")
			}
			if mount.Name != executorTokenVolume || container.Name == executorTokenWaiter {
				continue
			}
			for _, expected := range executorEnvironment {
				count := 0
				for _, env := range container.Env {
					if env.Name == expected[0] {
						count++
						if env.Value != expected[1] || env.ValueFrom != nil {
							return fmt.Errorf("executor container %q has unsafe %s", container.Name, expected[0])
						}
					}
				}
				if count != 1 {
					return fmt.Errorf("executor container %q must have exactly one %s", container.Name, expected[0])
				}
			}
			// Names alone do not identify Argo helpers: a resource Pod may
			// have authored containers named init or wait. Verify their image
			// and the position in which Argo actually creates each executor.
			if container.Image == argoworkflow.ExecutorImage && ((initializer && container.Name == "init") || (!initializer && container.Name == "wait")) {
				continue
			}
			if !initializer && resourcePod && container.Name == "main" && container.Image == argoworkflow.ExecutorImage {
				continue
			}
			return errors.New("pipeline container can access executor identity")
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
func (controller *Controller) executorIdentityPod(ctx context.Context, wf *wfv1.Workflow, serviceAccount string) (*corev1.Pod, error) {
	name := controller.identityPodName(wf, serviceAccount)
	desired := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: wf.Namespace, Labels: map[string]string{"oberth.ci/role": "executor-identity"}, OwnerReferences: []metav1.OwnerReference{{APIVersion: "argoproj.io/v1alpha1", Kind: "Workflow", Name: wf.Name, UID: wf.UID, Controller: ptr.To(true)}}},
		Spec:       corev1.PodSpec{ServiceAccountName: serviceAccount, AutomountServiceAccountToken: ptr.To(false), RestartPolicy: corev1.RestartPolicyNever, SchedulingGates: []corev1.PodSchedulingGate{{Name: "oberth.ci/executor-identity"}}, Containers: []corev1.Container{{Name: "identity", Image: controller.config.SourceSeedImage, Command: []string{"/bin/false"}, SecurityContext: nonrootContainerSecurity(65534), Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1m"), corev1.ResourceMemory: resource.MustParse("1Mi")}, Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("8Mi")}}}}},
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
	if pod.DeletionTimestamp != nil || pod.UID == "" || pod.Spec.ServiceAccountName != serviceAccount || pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken || len(pod.Spec.SchedulingGates) != 1 || pod.Spec.SchedulingGates[0].Name != "oberth.ci/executor-identity" || len(pod.Spec.Volumes) != 0 || len(pod.Spec.InitContainers) != 0 || len(pod.Spec.EphemeralContainers) != 0 || len(pod.Spec.Containers) != 1 || pod.Spec.Containers[0].Name != "identity" || pod.Spec.Containers[0].Image != controller.config.SourceSeedImage || len(pod.Spec.Containers[0].Command) != 1 || pod.Spec.Containers[0].Command[0] != "/bin/false" || len(pod.OwnerReferences) != 1 || pod.OwnerReferences[0].UID != wf.UID || pod.OwnerReferences[0].Kind != "Workflow" || pod.OwnerReferences[0].Controller == nil || !*pod.OwnerReferences[0].Controller {
		return nil, errors.New("executor identity lifetime Pod has unexpected ownership or configuration")
	}
	return pod, nil
}

// The authenticated bound-Pod name carries the Workflow UID AND name so the
// native task-result admission policy can verify both without an API lookup.
// Reserve this prefix against pipeline ServiceAccounts in the Pod policy.
func (controller *Controller) identityPodName(workflow *wfv1.Workflow, serviceAccount string) string {
	role := "executor"
	if serviceAccount != controller.config.ExecutorServiceAccount {
		digest := sha256.Sum256([]byte(serviceAccount))
		role = fmt.Sprintf("%x", digest[:12])
	}
	return fmt.Sprintf("oberth-executor.%s.%s.%s", workflow.UID, role, workflow.Name)
}

func (controller *Controller) retireExecutorIdentity(ctx context.Context, wf *wfv1.Workflow) error {
	if controller.kube == nil || wf.UID == "" {
		return nil
	}
	return controller.retireIdentityPod(ctx, wf, "")
}

func (controller *Controller) retireIdentityPod(ctx context.Context, wf *wfv1.Workflow, serviceAccount string) error {
	pods := controller.kube.CoreV1().Pods(wf.Namespace)
	holders, err := pods.List(ctx, metav1.ListOptions{LabelSelector: "oberth.ci/role=executor-identity"})
	if err != nil {
		return err
	}
	for _, pod := range holders.Items {
		// Only the holder shape matches: one scheduling gate and the single identity container, both fixed at creation.
		if len(pod.Spec.SchedulingGates) != 1 || len(pod.Spec.Containers) != 1 || pod.Spec.Containers[0].Name != "identity" || len(pod.OwnerReferences) != 1 || pod.OwnerReferences[0].UID != wf.UID || pod.OwnerReferences[0].Kind != "Workflow" || pod.OwnerReferences[0].Controller == nil || !*pod.OwnerReferences[0].Controller {
			continue
		}
		if serviceAccount != "" && pod.Spec.ServiceAccountName != serviceAccount {
			continue
		}
		err = pods.Delete(ctx, pod.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &pod.UID}, GracePeriodSeconds: ptr.To(int64(0))})
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// TokenRequest gets at most one re-request, only for explicit load/timeouts.
// Both calls and their backoff share a single 20-second per-Pod deadline.
func (controller *Controller) requestExecutorIdentity(ctx context.Context, namespace, account string, request *authenticationv1.TokenRequest, workflows ...string) (*authenticationv1.TokenRequest, error) {
	requestCtx, cancel := context.WithTimeout(ctx, executorIdentityAttemptTimeout)
	defer cancel()
	for attempt := 1; ; attempt++ {
		// The retained workflow lookup must run even after primary config removal.
		// Production always supplies the exact Workflow; journal-bound calls cannot
		// fall back to an unnamed legacy token request.

		token, err := controller.kube.CoreV1().ServiceAccounts(namespace).CreateToken(requestCtx, account, request, metav1.CreateOptions{})
		var network interface{ Timeout() bool }
		transient := apierrors.IsTooManyRequests(err) || apierrors.IsServiceUnavailable(err) || apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err) || errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &network) && network.Timeout())
		if err == nil || attempt == 2 || !transient || requestCtx.Err() != nil {
			return token, err
		}
		timer := time.NewTimer(executorIdentityBackoff(1))
		select {
		case <-requestCtx.Done():
			timer.Stop()
			return nil, requestCtx.Err()
		case <-timer.C:
		}
	}
}

// Argo adds shared scratch after admission. Only the exact native helper
// runtime mount is tolerated. Resource helpers cannot receive it: a writable
// argoexec could otherwise replace the operator executor before main starts.
func validArgoScratchMount(c corev1.Container, mount corev1.VolumeMount, initializer, resourcePod, server bool) bool {
	if server {
		return true
	} // Constructor and position already checked.
	executor := c.Image == argoworkflow.ExecutorImage && ((initializer && c.Name == "init") || (!initializer && c.Name == "wait") || (!initializer && resourcePod && c.Name == "main"))
	switch mount.Name {
	case "var-run-argo":
		expected := corev1.VolumeMount{Name: "var-run-argo", MountPath: "/var/run/argo", ReadOnly: initializer && !executor}
		return (!resourcePod || executor) && apiequality.Semantic.DeepEqual(mount, expected)
	case "argo-staging":
		return executor || (!initializer && !resourcePod && c.Name == "main" && apiequality.Semantic.DeepEqual(mount, corev1.VolumeMount{Name: "argo-staging", MountPath: "/argo/staging"}))
	default:
		return executor
	}
}

func argoScratchVolume(name string) bool {
	return name == "tmp-dir-argo" || name == "var-run-argo" || name == "argo-staging"
}
