package argojob

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	wfclientset "github.com/argoproj/argo-workflows/v4/pkg/client/clientset/versioned"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/oberthci/oberth/internal/argowatch"
	"github.com/oberthci/oberth/internal/runprogress"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/pipeline"
)

// The live suite runs against a real Argo Workflows controller. It is gated on
// an explicit kubeconfig rather than skipped by default for convenience: these
// tests create and delete objects in a cluster, so the cluster must be named
// deliberately. Point OBERTH_ARGO_LIVE_KUBECONFIG at a disposable cluster
// (hack/argo-live-cluster.sh builds one with kind) and set
// OBERTH_ARGO_LIVE_NAMESPACE if the controller does not watch "default".
//
// Everything these tests prove is also covered by the unit suite against the
// decoded object. What they add is that a real Argo controller accepts the
// object Oberth builds, schedules it, and reports a node tree the engine's
// status mapping reads correctly -- which no fake can establish.
const (
	liveKubeconfigEnvironment = "OBERTH_ARGO_LIVE_KUBECONFIG"
	liveNamespaceEnvironment  = "OBERTH_ARGO_LIVE_NAMESPACE"
)

type liveCluster struct {
	hub        *argowatch.Hub
	workflows  WorkflowClient
	kube       kubernetes.Interface
	restConfig *rest.Config
	namespace  string
}

func requireLiveCluster(t *testing.T) liveCluster {
	cluster, _, _ := startLiveWatchSession(t, t.Context(), liveClusterClients(t))
	return cluster
}

func liveClusterClients(t *testing.T) liveCluster {
	t.Helper()
	kubeconfig := strings.TrimSpace(os.Getenv(liveKubeconfigEnvironment))
	if kubeconfig == "" {
		t.Skipf("live Argo suite needs %s pointing at a disposable cluster (hack/argo-live-cluster.sh)", liveKubeconfigEnvironment)
	}
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		t.Fatalf("load %s: %v", kubeconfig, err)
	}
	config.Timeout = 30 * time.Second
	namespace := strings.TrimSpace(os.Getenv(liveNamespaceEnvironment))
	if namespace == "" {
		namespace = "default"
	}
	kube, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatalf("kubernetes client: %v", err)
	}
	argo, err := wfclientset.NewForConfig(config)
	if err != nil {
		t.Fatalf("argo client: %v", err)
	}
	return liveCluster{
		workflows:  argo.ArgoprojV1alpha1().Workflows(namespace),
		kube:       kube,
		restConfig: config,
		namespace:  namespace,
	}
}

func (cluster liveCluster) config() Config {
	return Config{
		Namespace:                  cluster.namespace,
		PipelineServiceAccount:     "oberth-argo-pipeline",
		CredentialedServiceAccount: "oberth-argo-credentialed",
		CISecretsServiceAccount:    "oberth-argo-ci-secrets",
		ExecutorServiceAccount:     "oberth-argo-executor",
		RunnerImagePrefixes:        []string{"golang:", "alpine:", "busybox:"},
	}
}

// ensureServiceAccounts creates the same four identities the chart's
// argo-identities.yaml declares, so the live suite exercises the real
// deployment shape.
func (cluster liveCluster) ensureServiceAccounts(t *testing.T, ctx context.Context) {
	t.Helper()
	for _, name := range []string{"oberth-argo-pipeline", "oberth-argo-credentialed", "oberth-argo-ci-secrets", "oberth-argo-executor"} {
		_, err := cluster.kube.CoreV1().ServiceAccounts(cluster.namespace).Create(ctx,
			&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name}}, metav1.CreateOptions{})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatalf("create ServiceAccount %s: %v", name, err)
		}
	}
	// Argo resolves the executor identity through a long-lived token Secret,
	// which Kubernetes 1.24+ no longer creates automatically.
	_, err := cluster.kube.CoreV1().Secrets(cluster.namespace).Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "oberth-argo-executor.service-account-token",
			Annotations: map[string]string{"kubernetes.io/service-account.name": "oberth-argo-executor"},
		},
		Type: corev1.SecretTypeServiceAccountToken,
	}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create executor token Secret: %v", err)
	}
	cluster.ensureExecutorRBAC(t, ctx)
}

func (cluster liveCluster) ensureExecutorRBAC(t *testing.T, ctx context.Context) {
	t.Helper()
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: "oberth-argo-executor"},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{"argoproj.io"},
			Resources: []string{"workflowtaskresults"},
			Verbs:     []string{"create", "patch"},
		}},
	}
	if _, err := cluster.kube.RbacV1().Roles(cluster.namespace).Create(ctx, role, metav1.CreateOptions{}); err != nil &&
		!apierrors.IsAlreadyExists(err) {
		t.Fatalf("create executor Role: %v", err)
	}
	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "oberth-argo-executor"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "oberth-argo-executor"},
		Subjects: []rbacv1.Subject{{
			Kind: "ServiceAccount", Name: "oberth-argo-executor", Namespace: cluster.namespace,
		}},
	}
	if _, err := cluster.kube.RbacV1().RoleBindings(cluster.namespace).Create(ctx, binding, metav1.CreateOptions{}); err != nil &&
		!apierrors.IsAlreadyExists(err) {
		t.Fatalf("create executor RoleBinding: %v", err)
	}
}

// TestLiveBranchSubmissionCannotReachTheCredentialedServiceAccount is the
// real-cluster form of the load-bearing property. The object the Argo
// controller actually stores must carry the pipeline identity and no
// ServiceAccount token, however the document asked.
func TestLiveBranchSubmissionCannotReachTheCredentialedServiceAccount(t *testing.T) {
	cluster := requireLiveCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cluster.ensureServiceAccounts(t, ctx)

	controller, err := NewController(cluster.workflows, cluster.kube, cluster.config())
	if err != nil {
		t.Fatalf("controller: %v", err)
	}
	seeder := NewSourceSeeder(cluster.kube, cluster.execStreamer(), cluster.config())
	controller = controller.WithSourceSeeder(seeder)
	request := liveRequest("oberth-live-identity", pipeline.TriggerCI, liveGreedyDocument)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
		defer cleanupCancel()
		_ = controller.Cancel(cleanupCtx, request.Name, request.RunID)
	})
	if _, err := controller.Create(ctx, request); err != nil {
		t.Fatalf("create: %v", err)
	}

	stored, err := cluster.workflows.Get(ctx, request.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if stored.Spec.ServiceAccountName != "oberth-argo-pipeline" {
		t.Fatalf("stored ServiceAccount = %q, want oberth-argo-pipeline", stored.Spec.ServiceAccountName)
	}
	if stored.Spec.AutomountServiceAccountToken == nil || *stored.Spec.AutomountServiceAccountToken {
		t.Fatal("stored Workflow automounts a ServiceAccount token on the branch tier")
	}
	for index, template := range stored.Spec.Templates {
		if template.ServiceAccountName != "oberth-argo-pipeline" {
			t.Errorf("stored template %d ServiceAccount = %q", index, template.ServiceAccountName)
		}
	}

	// The pod the controller actually schedules is the real proof: Kubernetes,
	// not Oberth, decides which identity a container runs as.
	pod := awaitRunPod(t, ctx, cluster, request.RunID)
	if pod.Spec.ServiceAccountName != "oberth-argo-pipeline" {
		t.Fatalf("scheduled Pod runs as %q, want oberth-argo-pipeline", pod.Spec.ServiceAccountName)
	}
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Fatal("scheduled Pod automounts a ServiceAccount token")
	}

	// This is the assertion the whole tier separation rests on: the step
	// container -- where the repository's own commands run, and where an
	// secretstore exec invocation would look for a token -- must have no
	// ServiceAccount token mounted anywhere in its filesystem. Argo's executor
	// gets one, under a different identity, in a different container.
	main := containerNamed(t, pod, mainContainerName)
	for _, mount := range main.VolumeMounts {
		if strings.Contains(mount.MountPath, "serviceaccount") ||
			mount.Name == "exec-sa-token" {
			t.Fatalf("the step container mounts %q at %q; a branch-tier step must hold no ServiceAccount token",
				mount.Name, mount.MountPath)
		}
	}

	// The executor does get a token, and it is a different identity.
	executorTokenMounted := false
	for _, container := range pod.Spec.InitContainers {
		for _, mount := range container.VolumeMounts {
			if mount.Name == "exec-sa-token" {
				executorTokenMounted = true
			}
		}
	}
	if !executorTokenMounted {
		t.Error("Argo's executor container has no token; the run would not be able to report results")
	}

	// The forced security baseline must survive Argo's own Pod construction.
	// Argo builds the Pod spec itself from the Workflow, so a field Oberth
	// sets is only real if it appears here.
	if pod.Spec.SecurityContext == nil ||
		pod.Spec.SecurityContext.SeccompProfile == nil ||
		pod.Spec.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("scheduled Pod has no RuntimeDefault seccomp profile: %+v", pod.Spec.SecurityContext)
	}
	if main.SecurityContext == nil {
		t.Fatal("the step container has no security context on the scheduled Pod")
	}
	if main.SecurityContext.AllowPrivilegeEscalation == nil || *main.SecurityContext.AllowPrivilegeEscalation {
		t.Error("the step container allows privilege escalation")
	}
	if main.SecurityContext.ReadOnlyRootFilesystem == nil || !*main.SecurityContext.ReadOnlyRootFilesystem {
		t.Error("the step container does not have a read-only root filesystem")
	}
}

// TestLiveReadOnlyRootFilesystemIsRealAndTmpIsWritable proves the forced
// baseline with a step that actually writes.
//
// The other live workflows only echo, so they would pass with a read-only root
// that was never exercised. This one writes to private scratch (which must succeed,
// because the Go toolchain writes to os.TempDir() on every build) and then to
// the root filesystem (which must fail). Without the writable private scratch the
// baseline would break every real pipeline; without the read-only root it
// would not be a baseline.
func TestLiveReadOnlyRootFilesystemIsRealAndTmpIsWritable(t *testing.T) {
	cluster := requireLiveCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cluster.ensureServiceAccounts(t, ctx)

	controller, err := NewController(cluster.workflows, cluster.kube, cluster.config())
	if err != nil {
		t.Fatalf("controller: %v", err)
	}
	controller = controller.WithSourceSeeder(NewSourceSeeder(cluster.kube, cluster.execStreamer(), cluster.config()))
	request := liveRequest("oberth-live-rootfs", pipeline.TriggerCI, liveFilesystemDocument)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
		defer cleanupCancel()
		_ = controller.Cancel(cleanupCtx, request.Name, request.RunID)
	})
	if _, err := controller.Create(ctx, request); err != nil {
		t.Fatalf("create: %v", err)
	}
	log := &recordingWriter{}
	completion, err := withWaitTestHub(t, controller, cluster.hub).Wait(ctx, request.Name, request.RunID, log)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if !completion.Succeeded {
		t.Fatalf("filesystem probe did not succeed: phase=%s reason=%s\n%s",
			completion.Phase, completion.Reason, truncate(log.String(), 4000))
	}
	body := log.String()
	if !strings.Contains(body, "tmp-write-ok") {
		t.Errorf("writing to /tmp failed; a read-only root would break every real build:\n%s", truncate(body, 4000))
	}
	if !strings.Contains(body, "root-write-refused") {
		t.Errorf("writing to the root filesystem succeeded; the read-only root is not in effect:\n%s", truncate(body, 4000))
	}
}

func containerNamed(t *testing.T, pod corev1.Pod, name string) corev1.Container {
	t.Helper()
	for _, container := range pod.Spec.Containers {
		if container.Name == name {
			return container
		}
	}
	t.Fatalf("Pod %s has no container named %q", pod.Name, name)
	return corev1.Container{}
}

// TestLiveWorkflowRunsToCompletion proves the whole supervision path against a
// real controller: submission, node-tree status mapping onto burns and steps,
// progress markers, pod log replay, and the terminal completion.
func TestLiveWorkflowRunsToCompletion(t *testing.T) {
	cluster := requireLiveCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cluster.ensureServiceAccounts(t, ctx)

	controller, err := NewController(cluster.workflows, cluster.kube, cluster.config())
	if err != nil {
		t.Fatalf("controller: %v", err)
	}
	controller = controller.WithSourceSeeder(NewSourceSeeder(cluster.kube, cluster.execStreamer(), cluster.config()))
	request := liveRequest("oberth-live-success", pipeline.TriggerCI, liveTwoStageDocument)
	request.SourceDir = writeCheckout(t)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
		defer cleanupCancel()
		_ = controller.Cancel(cleanupCtx, request.Name, request.RunID)
	})
	if _, err := controller.Create(ctx, request); err != nil {
		t.Fatalf("create: %v", err)
	}

	log := &recordingWriter{}
	completion, err := withWaitTestHub(t, controller, cluster.hub).Wait(ctx, request.Name, request.RunID, log)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if !completion.Succeeded {
		t.Fatalf("workflow did not succeed: phase=%s reason=%s", completion.Phase, completion.Reason)
	}
	if len(completion.Steps) < 3 {
		t.Fatalf("expected at least three step results, got %d: %+v", len(completion.Steps), completion.Steps)
	}

	// Every step is attributed to the burn (the nested DAG) that encloses it.
	stages := map[string][]string{}
	for _, step := range completion.Steps {
		stages[step.Stage] = append(stages[step.Stage], step.Step)
		if step.Status != runprogress.StepPassed {
			t.Errorf("step %s/%s = %s", step.Stage, step.Step, step.Status)
		}
	}
	for _, stage := range []string{"setup", "verify"} {
		if len(stages[stage]) == 0 {
			t.Errorf("no steps attributed to burn %q; observed %v", stage, stages)
		}
	}

	body := log.String()
	// Progress markers drive the dashboard and the step index.
	if strings.Contains(body, "@@oberth-step-progress@@") {
		t.Error("step control marker was written")
	}
	// Real pod output reached the run log, prefixed for the log index.
	if !strings.Contains(body, "hello-from-setup") {
		t.Error("the setup step's real Pod log did not reach the run log")
	}
	if !strings.Contains(body, "[verify/") {
		t.Errorf("verify burn output is not attributed to its burn; log:\n%s", truncate(body, 4000))
	}
}

// TestLiveFailingWorkflowReportsTheFailedStep proves a real non-zero exit maps
// back to a failed run naming the exact burn and step.
func TestLiveFailingWorkflowReportsTheFailedStep(t *testing.T) {
	cluster := requireLiveCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cluster.ensureServiceAccounts(t, ctx)

	controller, err := NewController(cluster.workflows, cluster.kube, cluster.config())
	if err != nil {
		t.Fatalf("controller: %v", err)
	}
	controller = controller.WithSourceSeeder(NewSourceSeeder(cluster.kube, cluster.execStreamer(), cluster.config()))
	request := liveRequest("oberth-live-failure", pipeline.TriggerCI, liveFailingDocument)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
		defer cleanupCancel()
		_ = controller.Cancel(cleanupCtx, request.Name, request.RunID)
	})
	if _, err := controller.Create(ctx, request); err != nil {
		t.Fatalf("create: %v", err)
	}
	completion, err := withWaitTestHub(t, controller, cluster.hub).Wait(ctx, request.Name, request.RunID, &recordingWriter{})
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if completion.Succeeded {
		t.Fatal("a workflow whose step exits non-zero reported success")
	}
	if completion.FailedStep == "" {
		t.Fatalf("no failed step was identified: %+v", completion)
	}
	found := false
	for _, step := range completion.Steps {
		if step.Status == runprogress.StepFailed && step.ExitCode != 0 {
			found = true
		}
	}
	if !found {
		t.Fatalf("no failed step carries a non-zero exit code: %+v", completion.Steps)
	}
}

// TestLiveTerminalStateReconstructsWithoutInProcessState proves restart
// recovery: a completed Workflow reports its real result to a controller that
// never saw it created.
func TestLiveTerminalStateReconstructsWithoutInProcessState(t *testing.T) {
	cluster := requireLiveCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cluster.ensureServiceAccounts(t, ctx)

	controller, err := NewController(cluster.workflows, cluster.kube, cluster.config())
	if err != nil {
		t.Fatalf("controller: %v", err)
	}
	controller = controller.WithSourceSeeder(NewSourceSeeder(cluster.kube, cluster.execStreamer(), cluster.config()))
	request := liveRequest("oberth-live-recovery", pipeline.TriggerCI, liveTwoStageDocument)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
		defer cleanupCancel()
		_ = controller.Cancel(cleanupCtx, request.Name, request.RunID)
	})
	if _, err := controller.Create(ctx, request); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := withWaitTestHub(t, controller, cluster.hub).Wait(ctx, request.Name, request.RunID, &recordingWriter{}); err != nil {
		t.Fatalf("wait: %v", err)
	}

	restarted, err := NewController(cluster.workflows, cluster.kube, cluster.config())
	if err != nil {
		t.Fatalf("restarted controller: %v", err)
	}
	owns, err := restarted.Owns(ctx, request.Name)
	if err != nil || !owns {
		t.Fatalf("Owns = %v,%v; want true", owns, err)
	}
	completion, err := restarted.TerminalState(ctx, request.Name)
	if err != nil {
		t.Fatalf("TerminalState: %v", err)
	}
	if !completion.Succeeded || len(completion.Steps) == 0 {
		t.Fatalf("reconstructed completion = %+v", completion)
	}

	if _, err := restarted.TerminalState(ctx, "oberth-does-not-exist"); !errors.Is(err, ErrNotTerminal) {
		t.Fatalf("absent Workflow = %v, want ErrNotTerminal", err)
	}
	if owns, err := restarted.Owns(ctx, "oberth-does-not-exist"); err != nil || owns {
		t.Fatalf("Owns(absent) = %v,%v", owns, err)
	}
}

// TestLiveCreateIsIdempotentForTheSameRun proves an ambiguous create can be
// retried, and that a name belonging to another run is never adopted.
func TestLiveCreateIsIdempotentForTheSameRun(t *testing.T) {
	cluster := requireLiveCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cluster.ensureServiceAccounts(t, ctx)

	controller, err := NewController(cluster.workflows, cluster.kube, cluster.config())
	if err != nil {
		t.Fatalf("controller: %v", err)
	}
	controller = controller.WithSourceSeeder(NewSourceSeeder(cluster.kube, cluster.execStreamer(), cluster.config()))
	request := liveRequest("oberth-live-idempotent", pipeline.TriggerCI, liveTwoStageDocument)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
		defer cleanupCancel()
		_ = controller.Cancel(cleanupCtx, request.Name, request.RunID)
	})
	if _, err := controller.Create(ctx, request); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if _, err := controller.Create(ctx, request); err != nil {
		t.Fatalf("identical re-create should be adopted: %v", err)
	}

	foreign := request
	foreign.RunID = "run-somebody-else"
	if _, err := controller.Create(ctx, foreign); err == nil {
		t.Fatal("a different run adopted an existing Workflow name")
	}
}

func awaitRunPod(t *testing.T, ctx context.Context, cluster liveCluster, runID string) corev1.Pod {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		pods, err := cluster.kube.CoreV1().Pods(cluster.namespace).List(ctx, metav1.ListOptions{
			LabelSelector: "oberth.ci/run=" + runID,
		})
		if err == nil && len(pods.Items) != 0 {
			return pods.Items[0]
		}
		select {
		case <-ctx.Done():
			t.Fatalf("context ended waiting for a Pod: %v", ctx.Err())
		case <-time.After(time.Second):
		}
	}
	t.Fatalf("no Pod appeared for run %s within the deadline", runID)
	return corev1.Pod{}
}

func liveRequest(name string, trigger pipeline.Trigger, source string) Request {
	return Request{
		RunID: name, Name: name, Repo: "oberth", UpstreamOrg: "acme",
		Ref: "refs/heads/argo-live", SHA: testSHA, Trigger: trigger,
		Source:          []byte(source),
		SourceDir:       "/tmp/oberth-live-test-src",
		ApprovedSecrets: map[string]bool{},
	}
}

// liveGreedyDocument asks for the release ServiceAccount at every level a real
// document can, and sleeps long enough for the Pod to be observed.
const liveGreedyDocument = `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  activeDeadlineSeconds: 180
  serviceAccountName: oberth-argo-credentialed
  automountServiceAccountToken: true
  templates:
    - name: main
      serviceAccountName: oberth-argo-credentialed
      automountServiceAccountToken: true
      container:
        image: busybox:1.37@sha256:ab33eacc8251e3807b85bb6dba570e4698c3998eca6f0fc2ccb60575a563ea74
        command: [/bin/sh, -c]
        args: ["sleep 45"]
`

// liveTwoBurnDocument is the burn/step shape Oberth's model expects: an
// entrypoint DAG of burns, each burn a nested DAG of steps.
const liveTwoStageDocument = `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  activeDeadlineSeconds: 300
  templates:
    - name: main
      dag:
        tasks:
          - name: setup
            template: setup-burn
          - name: verify
            depends: setup
            template: verify-burn
    - name: setup-burn
      dag:
        tasks:
          - name: prepare
            template: say
            arguments:
              parameters: [{name: message, value: hello-from-setup}]
    - name: verify-burn
      dag:
        tasks:
          - name: lint
            template: say
            arguments:
              parameters: [{name: message, value: hello-from-lint}]
          - name: test
            template: say
            arguments:
              parameters: [{name: message, value: hello-from-test}]
    - name: say
      inputs:
        parameters:
          - name: message
      container:
        image: busybox:1.37@sha256:ab33eacc8251e3807b85bb6dba570e4698c3998eca6f0fc2ccb60575a563ea74
        command: [/bin/echo]
        args: ["{{inputs.parameters.message}}"]
`

const liveFailingDocument = `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  activeDeadlineSeconds: 300
  templates:
    - name: main
      dag:
        tasks:
          - name: verify
            template: verify-burn
    - name: verify-burn
      dag:
        tasks:
          - name: failing-check
            template: boom
    - name: boom
      container:
        image: busybox:1.37@sha256:ab33eacc8251e3807b85bb6dba570e4698c3998eca6f0fc2ccb60575a563ea74
        command: [/bin/sh, -c]
        args: ["echo about-to-fail; exit 7"]
`

type recordingWriter struct {
	body []byte
}

func (writer *recordingWriter) Write(chunk []byte) (int, error) {
	writer.body = append(writer.body, chunk...)
	return len(chunk), nil
}

func (writer *recordingWriter) String() string { return string(writer.body) }

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "\n...[truncated]"
}

// liveFilesystemDocument probes the forced filesystem baseline from inside a
// real step: private scratch must be writable, the root filesystem must not be.
const liveFilesystemDocument = `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  activeDeadlineSeconds: 300
  templates:
    - name: main
      dag:
        tasks:
          - name: filesystem
            template: probe-burn
    - name: probe-burn
      dag:
        tasks:
          - name: probe
            template: probe
    - name: probe
      container:
        image: busybox:1.37@sha256:ab33eacc8251e3807b85bb6dba570e4698c3998eca6f0fc2ccb60575a563ea74
        command: [/bin/sh, -c]
        args:
          - |
            if echo ok > /tmp/oberth-probe; then echo tmp-write-ok; else echo tmp-write-FAILED; exit 1; fi
            if echo no > /oberth-probe 2>/dev/null; then echo root-write-SUCCEEDED; exit 1; else echo root-write-refused; fi
`

// liveExecStreamer is the same exec transport the server wires in production,
// so the live suite seeds a run's source exactly the way a real install does
// instead of relying on a claim someone filled by hand beforehand.
func (cluster liveCluster) execStreamer() ExecStreamer {
	return func(ctx context.Context, namespace, pod, container string, command []string,
		stdin io.Reader, stdout, stderr io.Writer,
	) error {
		request := cluster.kube.CoreV1().RESTClient().Post().
			Resource("pods").Namespace(namespace).Name(pod).SubResource("exec").
			VersionedParams(&corev1.PodExecOptions{
				Container: container, Command: command,
				Stdin: stdin != nil, Stdout: stdout != nil, Stderr: stderr != nil,
			}, scheme.ParameterCodec)
		executor, err := remotecommand.NewSPDYExecutor(cluster.restConfig, http.MethodPost, request.URL())
		if err != nil {
			return err
		}
		return executor.StreamWithContext(ctx, remotecommand.StreamOptions{
			Stdin: stdin, Stdout: stdout, Stderr: stderr,
		})
	}
}

// TestLiveExecutorIdentityDefaulting proves delivery against actual controller
// Pods and API defaults. It submits exactly two Workflows; no source claim is
// needed for these commands, so the same Build and delivery paths run directly.
func TestLiveExecutorIdentityDefaulting(t *testing.T) {
	liveExecutorIdentityProof(t, []string{"ordinary", "resource"})
}

func TestLiveExecutorIdentityAuthoredInit(t *testing.T) {
	liveExecutorIdentityProof(t, []string{"ordinary-init"})
}

func liveExecutorIdentityProof(t *testing.T, shapes []string) {
	t.Helper()
	cluster := requireLiveCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	cluster.ensureServiceAccounts(t, ctx)
	config := cluster.config()
	grant := &argoworkflow.Capability{Namespace: cluster.namespace, ServiceAccount: "oberth-live-resource", ResourceKinds: []argoworkflow.ResourceKindCapability{{APIVersion: "v1", Kind: "ConfigMap", Actions: []string{"get"}}}}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	_, err := cluster.kube.CoreV1().ServiceAccounts(cluster.namespace).Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: grant.ServiceAccount}}, metav1.CreateOptions{})
	must(err)
	_, err = cluster.kube.CoreV1().Secrets(cluster.namespace).Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: grant.ServiceAccount + ".service-account-token", Annotations: map[string]string{"kubernetes.io/service-account.name": grant.ServiceAccount}}, Type: corev1.SecretTypeServiceAccountToken}, metav1.CreateOptions{})
	must(err)
	_, err = cluster.kube.RbacV1().Roles(cluster.namespace).Create(ctx, &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: grant.ServiceAccount}, Rules: []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"configmaps"}, ResourceNames: []string{"oberth-live-proof"}, Verbs: []string{"get"}}, {APIGroups: []string{"argoproj.io"}, Resources: []string{"workflowtaskresults"}, Verbs: []string{"create", "patch"}}}}, metav1.CreateOptions{})
	must(err)
	_, err = cluster.kube.RbacV1().RoleBindings(cluster.namespace).Create(ctx, &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: grant.ServiceAccount}, RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: grant.ServiceAccount}, Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: grant.ServiceAccount, Namespace: cluster.namespace}}}, metav1.CreateOptions{})
	must(err)
	_, err = cluster.kube.CoreV1().ConfigMaps(cluster.namespace).Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "oberth-live-proof"}, Data: map[string]string{"proof": "resource-identity"}}, metav1.CreateOptions{})
	must(err)
	for _, shape := range shapes {
		t.Run(shape, func(t *testing.T) {
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			evidence := t.ArtifactDir()
			t.Cleanup(func() {
				if !t.Failed() {
					return
				}
				diagnosticCtx, stop := context.WithTimeout(context.Background(), 15*time.Second)
				defer stop()
				pods, listErr := cluster.kube.CoreV1().Pods(cluster.namespace).List(diagnosticCtx, metav1.ListOptions{LabelSelector: "workflows.argoproj.io/workflow=oberth-live-token-" + shape})
				if listErr != nil {
					t.Logf("read failed Pods: %v", listErr)
					return
				}
				for _, pod := range pods.Items {
					for _, c := range append(pod.Spec.InitContainers, pod.Spec.Containers...) {
						t.Logf("target %s/%s image=%s mounts=%v", pod.Name, c.Name, c.Image, c.VolumeMounts)
					}
				}
				body, marshalErr := json.MarshalIndent(pods, "", "  ")
				if marshalErr == nil {
					if writeErr := os.WriteFile(filepath.Join(evidence, shape+"-failed-pods.json"), body, 0o600); writeErr != nil {
						t.Log(writeErr)
					}
				}
			})
			source := `apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  activeDeadlineSeconds: 300
  templates:
  - name: main
    container:
      image: busybox:1.37@sha256:ab33eacc8251e3807b85bb6dba570e4698c3998eca6f0fc2ccb60575a563ea74
      workingDir: /tmp
      command: [sh, -c]
      args: ["echo ordinary-identity-proof"]
`
			if shape == "resource" {
				source = fmt.Sprintf(`apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  activeDeadlineSeconds: 300
  templates:
  - name: main
    resource:
      action: get
      manifest: |
        apiVersion: v1
        kind: ConfigMap
        metadata:
          name: oberth-live-proof
          namespace: %s
`, cluster.namespace)
			}
			if shape == "ordinary-init" {
				source = strings.Replace(source, "    container:\n", `    initContainers:
    - name: authored-init
      image: busybox:1.37@sha256:ab33eacc8251e3807b85bb6dba570e4698c3998eca6f0fc2ccb60575a563ea74
      command: [sh, -c]
      args:
      - |
        test -r /var/run/argo/template || exit 70
        if (printf tampered > /var/run/argo/template) 2>/dev/null; then exit 71; fi
        echo authored-init-read-only-proof
    container:
`, 1)
			}
			request := liveRequest("oberth-live-token-"+shape, pipeline.TriggerCI, source)
			request.RepositoryID = 1
			if shape == "resource" {
				request.Capability = grant
			}
			workflow, buildErr := Build(config, request)
			must(buildErr)
			controller, controllerErr := NewController(cluster.workflows, cluster.kube, config)
			must(controllerErr)
			controller = controller.WithWorkflowCapabilityLoader(func(context.Context, int64) (*argoworkflow.Capability, error) { return grant, nil })
			var deliveries atomic.Int32
			streamer := cluster.execStreamer()
			controller = controller.WithSourceSeeder(NewSourceSeeder(cluster.kube, func(ctx context.Context, ns, pod, container string, command []string, stdin io.Reader, stdout, stderr io.Writer) error {
				err := streamer(ctx, ns, pod, container, command, stdin, stdout, stderr)
				if container == executorTokenWaiter && err == nil {
					deliveries.Add(1)
				}
				return err
			}, config))
			_, createErr := cluster.workflows.Create(ctx, workflow, metav1.CreateOptions{})
			must(createErr)
			completion, waitErr := controller.Wait(ctx, request.Name, request.RunID, &recordingWriter{})
			must(waitErr)
			if !completion.Succeeded || deliveries.Load() != 1 {
				t.Fatalf("phase=%s reason=%s deliveries=%d", completion.Phase, completion.Reason, deliveries.Load())
			}
			pods, listErr := cluster.kube.CoreV1().Pods(cluster.namespace).List(ctx, metav1.ListOptions{LabelSelector: "workflows.argoproj.io/workflow=" + request.Name})
			must(listErr)
			checked := 0
			for i := range pods.Items {
				pod := &pods.Items[i]
				if pod.Labels["oberth.ci/role"] == "executor-identity" {
					continue
				}
				for _, c := range pod.Spec.InitContainers {
					if shape == "ordinary-init" && c.Name == "authored-init" {
						found := false
						for _, m := range c.VolumeMounts {
							if m.Name == "var-run-argo" && m.MountPath == "/var/run/argo" && m.ReadOnly {
								found = true
							}
						}
						if !found {
							t.Fatal("real authored init runtime mount is not read-only")
						}
						t.Log("actual API Pod: authored-init var-run-argo is read-only; attempted template write denied and initializer completed")
					}
					if c.Name == executorTokenWaiter {
						checked++
						if c.ImagePullPolicy == "" || c.TerminationMessagePath == "" || c.TerminationMessagePolicy == "" {
							t.Fatal("real API defaults were not observed")
						}
						must(validateExecutorTokenTarget(pod, controller.config.SourceSeedImage, shape == "resource"))
						t.Logf("Pod %s: defaulted waiter policy=%s path=%s termination=%s; token deliveries=%d; Workflow %s", pod.Name, c.ImagePullPolicy, c.TerminationMessagePath, c.TerminationMessagePolicy, deliveries.Load(), completion.Phase)
					}
				}
			}
			if checked != 1 {
				t.Fatalf("checked %d workflow Pods, want one", checked)
			}
			// Pod specs contain references and public trust, never delivered token
			// data. Keep them with go test -artifacts for the testdata replay.
			body, marshalErr := json.MarshalIndent(pods, "", "  ")
			must(marshalErr)
			must(os.WriteFile(filepath.Join(evidence, shape+"-pods.json"), body, 0o600))
			stored, getErr := cluster.workflows.Get(ctx, request.Name, metav1.GetOptions{})
			must(getErr)
			body, marshalErr = json.MarshalIndent(stored, "", "  ")
			must(marshalErr)
			must(os.WriteFile(filepath.Join(evidence, shape+"-workflow.json"), body, 0o600))
		})
	}
}

// The live-watch lane is an explicit local fault qualification, separate from
// the ordinary live suite. No other context or existing cluster is accepted.
func TestLiveWatchLifecycle(t *testing.T) {
	args := flag.Args()
	if len(args) == 4 && args[0] == "live-watch-child" {
		runLiveWatchChild(t, args[1], args[2], args[3])
		return
	}
	if len(args) != 1 || args[0] != "live-watch-faults" {
		t.Skip("live-watch lane needs -args live-watch-faults and the disposable harness kubeconfig")
	}
	t.Run("constant_request_budget", testLiveWatchRequestBudget)
	t.Run("control_plane_restart", testLiveWatchControlPlaneRestart)
	t.Run("server_process_restart_adopts", testLiveWatchServerRestart)
	t.Run("cancel_during_reconnect", testLiveWatchCancelReconnect)
}

type liveWatchSnapshot struct {
	WorkflowGETs    int64 `json:"workflow_gets"`
	PodLISTs        int64 `json:"pod_lists"`
	Watches         int64 `json:"watch_attempts"`
	WorkflowDeletes int64 `json:"workflow_deletes"`
}
type liveWatchCounter struct {
	workflowGets, podLists, watches, workflowDeletes atomic.Int64
	mu                                               sync.Mutex
	blocked                                          bool
	streams                                          map[*liveWatchBody]struct{}
	reconnect                                        chan struct{}
}

func (c *liveWatchCounter) snapshot() liveWatchSnapshot {
	return liveWatchSnapshot{c.workflowGets.Load(), c.podLists.Load(), c.watches.Load(), c.workflowDeletes.Load()}
}
func (c *liveWatchCounter) blockWatches(block bool) {
	c.mu.Lock()
	c.blocked = block
	var streams []*liveWatchBody
	if block {
		for stream := range c.streams {
			streams = append(streams, stream)
		}
	}
	c.mu.Unlock()
	for _, stream := range streams {
		_ = stream.Close()
	}
}

type liveWatchTransport struct {
	next    http.RoundTripper
	counter *liveWatchCounter
}
type liveWatchBody struct {
	io.ReadCloser
	counter *liveWatchCounter
	once    sync.Once
	err     error
}

func (b *liveWatchBody) Close() error {
	b.once.Do(func() {
		b.counter.mu.Lock()
		delete(b.counter.streams, b)
		b.counter.mu.Unlock()
		b.err = b.ReadCloser.Close()
	})
	return b.err
}
func (transport *liveWatchTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c := transport.counter
	watch := r.URL.Query().Get("watch") == "true" || r.URL.Query().Get("watch") == "1"
	if r.Method == http.MethodGet {
		switch {
		case watch:
			c.watches.Add(1)
		case strings.Contains(r.URL.Path, "/apis/argoproj.io/") && strings.Contains(r.URL.Path, "/workflows/"):
			c.workflowGets.Add(1)
		case strings.HasSuffix(r.URL.Path, "/pods"):
			c.podLists.Add(1)
		}
	}
	if r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/workflows/") {
		c.workflowDeletes.Add(1)
	}
	c.mu.Lock()
	blocked := c.blocked
	c.mu.Unlock()
	if watch && blocked {
		select {
		case c.reconnect <- struct{}{}:
		default:
		}
		return nil, errors.New("disposable live-watch reconnect fault")
	}
	response, err := transport.next.RoundTrip(r)
	if err != nil || !watch || response.Body == nil {
		return response, err
	}
	body := &liveWatchBody{ReadCloser: response.Body, counter: c}
	response.Body = body
	c.mu.Lock()
	blocked = c.blocked
	if !blocked {
		c.streams[body] = struct{}{}
	}
	c.mu.Unlock()
	if blocked {
		_ = body.Close()
		return nil, errors.New("disposable live-watch stream fault")
	}
	return response, nil
}

func startLiveWatchSession(t *testing.T, parent context.Context, raw liveCluster) (liveCluster, func(), *liveWatchCounter) {
	t.Helper()
	counter := &liveWatchCounter{streams: map[*liveWatchBody]struct{}{}, reconnect: make(chan struct{}, 1)}
	config := rest.CopyConfig(raw.restConfig)
	// Streaming watches use the session lifetime; individual live gates have
	// their caller deadlines. This prevents a transport timeout from polling.
	config.Timeout = 0
	config.Wrap(func(next http.RoundTripper) http.RoundTripper {
		return &liveWatchTransport{next: next, counter: counter}
	})
	kube, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	argo, err := wfclientset.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	hub, err := argowatch.New(kube, argo, raw.namespace, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() { defer close(done); hub.Run(ctx) }()
	var once sync.Once
	stop := func() { once.Do(func() { cancel(); <-done }) }
	t.Cleanup(stop)
	syncCtx, stopSync := context.WithTimeout(ctx, time.Minute)
	defer stopSync()
	if err := hub.WaitForSync(syncCtx); err != nil {
		t.Fatal(err)
	}
	return liveCluster{hub: hub, workflows: argo.ArgoprojV1alpha1().Workflows(raw.namespace), kube: kube, restConfig: config, namespace: raw.namespace}, stop, counter
}

type liveWatchSubscription struct {
	ObservationHub
	count atomic.Int64
}

func (h *liveWatchSubscription) Subscribe(name string) (<-chan struct{}, func()) {
	h.count.Add(1)
	wake, cancel := h.ObservationHub.Subscribe(name)
	var once sync.Once
	return wake, func() { once.Do(func() { cancel(); h.count.Add(-1) }) }
}
func liveWatchController(t *testing.T, cluster liveCluster) (*Controller, *liveWatchSubscription) {
	t.Helper()
	controller, err := NewController(cluster.workflows, cluster.kube, cluster.config())
	if err != nil {
		t.Fatal(err)
	}
	hub := &liveWatchSubscription{ObservationHub: cluster.hub}
	controller.WithSourceSeeder(NewSourceSeeder(cluster.kube, cluster.execStreamer(), cluster.config())).WithWatchHub(hub)
	return controller, hub
}

// Verify the dedicated kubeconfig, loopback endpoint and kind container label
// before any Kubernetes write or container restart. Never consult default context.
func requireLiveWatchKind(t *testing.T, raw liveCluster) string {
	t.Helper()
	loaded, err := clientcmd.LoadFromFile(os.Getenv(liveKubeconfigEnvironment))
	if err != nil {
		t.Fatal(err)
	}
	contextName := loaded.CurrentContext
	if !strings.HasPrefix(contextName, "kind-oberth-") {
		t.Fatal("live-watch lane requires its disposable kind context")
	}
	name := strings.TrimPrefix(contextName, "kind-")
	endpoint, err := url.Parse(raw.restConfig.Host)
	if err != nil {
		t.Fatal(err)
	}
	ip := net.ParseIP(endpoint.Hostname())
	if ip == nil || !ip.IsLoopback() {
		t.Fatal("live-watch lane requires a loopback kind endpoint")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	clusters, err := exec.CommandContext(ctx, "kind", "get", "clusters").Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(clusters)) != name {
		t.Fatal("live-watch lane requires exactly its own kind cluster")
	}
	nodes, err := exec.CommandContext(ctx, "kind", "get", "nodes", "--name", name).Output()
	if err != nil {
		t.Fatal(err)
	}
	node := name + "-control-plane"
	if strings.TrimSpace(string(nodes)) != node {
		t.Fatal("live-watch lane requires one control-plane node")
	}
	label, err := exec.CommandContext(ctx, "docker", "inspect", "--format", `{{index .Config.Labels "io.x-k8s.kind.cluster"}}`, node).Output()
	if err != nil || strings.TrimSpace(string(label)) != name {
		t.Fatal("container does not belong to the disposable kind cluster", err)
	}
	t.Logf("fault boundary: context=%s node=%s endpoint=%s", contextName, node, endpoint.Host)
	return node
}

func liveWatchRequest(name string, seconds int, onExit bool) Request {
	exit := ""
	if onExit {
		exit = "  onExit: cleanup\n"
	}
	source := fmt.Sprintf(`apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  activeDeadlineSeconds: 240
%s  templates:
  - name: main
    container:
      image: busybox:1.37@sha256:ab33eacc8251e3807b85bb6dba570e4698c3998eca6f0fc2ccb60575a563ea74
      workingDir: /tmp
      command: [sh, -c]
      args: ["echo watch-start; sleep %d; echo watch-finished"]
  - name: cleanup
    container:
      image: busybox:1.37@sha256:ab33eacc8251e3807b85bb6dba570e4698c3998eca6f0fc2ccb60575a563ea74
      workingDir: /tmp
      command: [sh, -c]
      args: ["sleep 2; echo watch-exit-handler"]
`, exit, seconds)
	return liveRequest(name, pipeline.TriggerCI, source)
}
func createLiveWatchWorkflow(t *testing.T, ctx context.Context, raw liveCluster, request Request) *wfv1.Workflow {
	t.Helper()
	raw.ensureServiceAccounts(t, ctx)
	workflow, err := Build(raw.config(), request)
	if err != nil {
		t.Fatal(err)
	}
	workflow, err = raw.workflows.Create(ctx, workflow, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		controller, err := NewController(raw.workflows, raw.kube, raw.config())
		if err == nil {
			_ = controller.Cancel(cleanup, workflow.Name, request.RunID)
		}
	})
	return workflow
}
func awaitLiveWatch(t *testing.T, ctx context.Context, predicate func() bool) {
	t.Helper()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for !predicate() {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
}
func liveWatchMainRunning(hub ObservationHub, name string) bool {
	for _, pod := range hub.PodsOf(name) {
		for _, status := range pod.Status.ContainerStatuses {
			if status.Name == mainContainerName && status.State.Running != nil {
				return true
			}
		}
	}
	return false
}

type liveWatchRun struct {
	done   chan struct{}
	result waitResult
}

func startLiveWatchRun(t *testing.T, parent context.Context, controller *Controller, request Request) *liveWatchRun {
	t.Helper()
	ctx, cancel := context.WithCancel(parent)
	run := &liveWatchRun{done: make(chan struct{})}
	go func() {
		defer close(run.done)
		run.result.completion, run.result.err = controller.Wait(ctx, request.Name, request.RunID, io.Discard)
	}()
	t.Cleanup(func() { cancel(); <-run.done })
	return run
}
func liveWatchCompletion(t *testing.T, ctx context.Context, run *liveWatchRun) Completion {
	t.Helper()
	select {
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	case <-run.done:
	}
	if run.result.err != nil {
		t.Fatal("watch observer failed", run.result.err)
	}
	return run.result.completion
}
func assertLiveWatchVerdict(t *testing.T, ctx context.Context, raw liveCluster, wf *wfv1.Workflow, result Completion, hub *liveWatchSubscription) {
	t.Helper()
	current, err := raw.workflows.Get(ctx, wf.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if current.UID != wf.UID || current.DeletionTimestamp != nil || !current.Status.Phase.Completed() || !result.Terminal || result.Phase != string(current.Status.Phase) || result.Succeeded != (current.Status.Phase == wfv1.WorkflowSucceeded) || hub.count.Load() != 0 {
		t.Fatalf("live Argo verdict/identity/subscription mismatch: native=%s result=%+v subscriptions=%d", current.Status.Phase, result, hub.count.Load())
	}
	t.Logf("verdict: uid=%s phase=%s subscriptions=%d", current.UID, result.Phase, hub.count.Load())
}

func testLiveWatchRequestBudget(t *testing.T) {
	raw := liveClusterClients(t)
	requireLiveWatchKind(t, raw)
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Minute)
	defer cancel()
	session, _, counter := startLiveWatchSession(t, ctx, raw)
	controller, hub := liveWatchController(t, session)
	request := liveWatchRequest(fmt.Sprintf("oberth-watch-budget-%d", time.Now().UnixNano()), 60, false)
	wf := createLiveWatchWorkflow(t, ctx, raw, request)
	run := startLiveWatchRun(t, ctx, controller, request)
	awaitLiveWatch(t, ctx, func() bool { return liveWatchMainRunning(session.hub, wf.Name) })
	started := time.Now()
	time.Sleep(15 * time.Second)
	early := counter.snapshot()
	time.Sleep(30 * time.Second)
	late := counter.snapshot()
	select {
	case <-run.done:
		t.Fatal("run ended before the request-rate comparison window")
	default:
	}
	if early.WorkflowGETs != late.WorkflowGETs || early.PodLISTs != late.PodLISTs {
		t.Fatal("observation traffic grew with idle run time", early, late)
	}
	result := liveWatchCompletion(t, ctx, run)
	final := counter.snapshot()
	if final.WorkflowGETs != late.WorkflowGETs+1 || final.PodLISTs != late.PodLISTs+1 || final.WorkflowGETs > 8 || final.PodLISTs > 3 || final.Watches < 2 || final.WorkflowDeletes != 0 {
		t.Fatal("completion was not one live Workflow GET and Pod snapshot", early, late, final)
	}
	assertLiveWatchVerdict(t, ctx, raw, wf, result, hub)
	if !result.Succeeded {
		t.Fatal("normal measured workflow did not succeed", result)
	}
	body, _ := json.Marshal(struct {
		Early, Late, Final liveWatchSnapshot
		Seconds            float64
	}{early, late, final, time.Since(started).Seconds()})
	t.Logf("request measurement: %s", body)
}
func testLiveWatchControlPlaneRestart(t *testing.T) {
	raw := liveClusterClients(t)
	node := requireLiveWatchKind(t, raw)
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Minute)
	defer cancel()
	session, _, counter := startLiveWatchSession(t, ctx, raw)
	controller, hub := liveWatchController(t, session)
	request := liveWatchRequest(fmt.Sprintf("oberth-watch-api-%d", time.Now().UnixNano()), 60, false)
	wf := createLiveWatchWorkflow(t, ctx, raw, request)
	run := startLiveWatchRun(t, ctx, controller, request)
	awaitLiveWatch(t, ctx, func() bool { return liveWatchMainRunning(session.hub, wf.Name) })
	before := counter.snapshot()
	output, err := exec.CommandContext(ctx, "docker", "restart", "--time", "1", node).CombinedOutput()
	if err != nil {
		t.Fatalf("restart disposable control plane: %v %s", err, output)
	}
	result := liveWatchCompletion(t, ctx, run)
	if counter.snapshot().Watches <= before.Watches || counter.snapshot().WorkflowDeletes != 0 {
		t.Fatal("watch did not reconnect or observer deleted Workflow", before, counter.snapshot())
	}
	assertLiveWatchVerdict(t, ctx, raw, wf, result, hub)
	t.Logf("control-plane restart: %s; watch attempts %d -> %d", strings.TrimSpace(string(output)), before.Watches, counter.snapshot().Watches)
}
func testLiveWatchCancelReconnect(t *testing.T) {
	raw := liveClusterClients(t)
	requireLiveWatchKind(t, raw)
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Minute)
	defer cancel()
	session, _, counter := startLiveWatchSession(t, ctx, raw)
	controller, hub := liveWatchController(t, session)
	request := liveWatchRequest(fmt.Sprintf("oberth-watch-stop-%d", time.Now().UnixNano()), 120, true)
	wf := createLiveWatchWorkflow(t, ctx, raw, request)
	run := startLiveWatchRun(t, ctx, controller, request)
	awaitLiveWatch(t, ctx, func() bool { return liveWatchMainRunning(session.hub, wf.Name) })
	counter.blockWatches(true)
	defer counter.blockWatches(false)
	select {
	case <-counter.reconnect:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	awaitLiveWatch(t, ctx, func() bool { return !session.hub.Healthy() })
	if err := controller.Cancel(ctx, wf.Name, request.RunID); err != nil {
		t.Fatal(err)
	}
	stopped, err := raw.workflows.Get(ctx, wf.Name, metav1.GetOptions{})
	if err != nil || stopped.UID != wf.UID || stopped.DeletionTimestamp != nil || stopped.Spec.Shutdown != wfv1.ShutdownStrategyStop {
		t.Fatal("cancel did not preserve the Workflow with native Stop", stopped, err)
	}
	select {
	case <-run.done:
		t.Fatal("observer concluded from an unavailable watch")
	default:
	}
	counter.blockWatches(false)
	result := liveWatchCompletion(t, ctx, run)
	assertLiveWatchVerdict(t, ctx, raw, wf, result, hub)
	final, err := raw.workflows.Get(ctx, wf.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	exited := false
	for _, node := range final.Status.Nodes {
		if node.TemplateName == "cleanup" && node.Phase == wfv1.NodeSucceeded {
			exited = true
		}
	}
	if !exited || counter.snapshot().WorkflowDeletes != 0 {
		t.Fatal("Stop lost its exit handler or deleted the Workflow", final.Status.Nodes, counter.snapshot())
	}
}

// A child process hosts precisely the server's Argo controller/hub lifetime.
// Killing it drops every observer map and connection; the replacement uses the
// public adoption API after a new cache synchronization, without another Create.
func runLiveWatchChild(t *testing.T, mode, name, directory string) {
	raw := liveClusterClients(t)
	requireLiveWatchKind(t, raw)
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Minute)
	defer cancel()
	session, _, _ := startLiveWatchSession(t, ctx, raw)
	controller, hub := liveWatchController(t, session)
	request := liveWatchRequest(name, 60, false)
	cached, found := session.hub.Workflow(name)
	if !found {
		t.Fatal("synced child cache lost existing Workflow")
	}
	var result Completion
	var err error
	switch mode {
	case "wait":
		run := startLiveWatchRun(t, ctx, controller, request)
		awaitLiveWatch(t, ctx, func() bool { return liveWatchMainRunning(session.hub, name) })
		if err = os.WriteFile(filepath.Join(directory, "ready"), []byte(cached.UID), 0600); err != nil {
			t.Fatal(err)
		}
		result = liveWatchCompletion(t, ctx, run)
	case "adopt":
		binding, ok, adoptErr := controller.Adopt(ctx, AdoptionRequest{Name: name, RunID: name, Ref: request.Ref, SHA: request.SHA, Trigger: request.Trigger, RunTrigger: argoworkflow.RunTriggerBranch})
		if adoptErr != nil || !ok || binding.UID != cached.UID {
			t.Fatal("replacement refused original cached Workflow", binding, ok, adoptErr)
		}
		if err = os.WriteFile(filepath.Join(directory, "adopted"), []byte(binding.UID), 0600); err != nil {
			t.Fatal(err)
		}
		result, err = controller.WaitResumed(ctx, name, name, io.Discard, nil, 0, binding)
		if err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal("unknown live-watch child mode")
	}
	assertLiveWatchVerdict(t, ctx, raw, cached, result, hub)
	if !result.Succeeded {
		t.Fatal(result)
	}
	body, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(directory, "result"), body, 0600); err != nil {
		t.Fatal(err)
	}
}
func testLiveWatchServerRestart(t *testing.T) {
	raw := liveClusterClients(t)
	requireLiveWatchKind(t, raw)
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Minute)
	defer cancel()
	request := liveWatchRequest(fmt.Sprintf("oberth-watch-process-%d", time.Now().UnixNano()), 60, false)
	wf := createLiveWatchWorkflow(t, ctx, raw, request)
	directory := t.TempDir()
	var beforeLog bytes.Buffer
	first := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLiveWatchLifecycle$", "-test.v", "-test.timeout=6m", "--", "live-watch-child", "wait", wf.Name, directory)
	first.Stdout = &beforeLog
	first.Stderr = &beforeLog
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	firstDone := make(chan error, 1)
	go func() { firstDone <- first.Wait() }()
	joined := false
	defer func() {
		if !joined {
			_ = first.Process.Kill()
			<-firstDone
		}
	}()
	awaitLiveWatch(t, ctx, func() bool {
		select {
		case err := <-firstDone:
			joined = true
			t.Fatalf("first observer exited before restart: %v %s", err, beforeLog.String())
		default:
		}
		_, err := os.Stat(filepath.Join(directory, "ready"))
		return err == nil
	})
	if err := first.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := <-firstDone; err == nil {
		t.Fatal("observer process was not killed")
	}
	joined = true
	t.Logf("terminated observer pid=%d; output: %s", first.Process.Pid, beforeLog.String())
	preserved, err := raw.workflows.Get(ctx, wf.Name, metav1.GetOptions{})
	if err != nil || preserved.UID != wf.UID || preserved.DeletionTimestamp != nil || preserved.Spec.Shutdown != "" {
		t.Fatal("process exit changed native execution", preserved, err)
	}
	second := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLiveWatchLifecycle$", "-test.v", "-test.timeout=6m", "--", "live-watch-child", "adopt", wf.Name, directory)
	output, err := second.CombinedOutput()
	t.Logf("replacement observer output: %s", output)
	if err != nil {
		t.Fatal("replacement observer failed", err)
	}
	adopted, err := os.ReadFile(filepath.Join(directory, "adopted"))
	if err != nil || string(adopted) != string(wf.UID) {
		t.Fatal("replacement did not adopt original UID", string(adopted), err)
	}
	body, err := os.ReadFile(filepath.Join(directory, "result"))
	if err != nil {
		t.Fatal(err)
	}
	var result Completion
	if err = json.Unmarshal(body, &result); err != nil || !result.Succeeded {
		t.Fatal(result, err)
	}
	final, err := raw.workflows.Get(ctx, wf.Name, metav1.GetOptions{})
	if err != nil || final.UID != wf.UID || final.Status.Phase != wfv1.WorkflowSucceeded {
		t.Fatal("restart changed the submitted Workflow", final, err)
	}
}
