package argojob

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

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

	"github.com/oberthci/oberth/internal/runprogress"
	"github.com/oberthci/oberth/pkg/periapsis"
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
	workflows  WorkflowClient
	kube       kubernetes.Interface
	restConfig *rest.Config
	namespace  string
}

func requireLiveCluster(t *testing.T) liveCluster {
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
		WorkflowTimeout:            10 * time.Minute,
		TTLSeconds:                 120,
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
	request := liveRequest("oberth-live-identity", periapsis.TriggerCI, liveGreedyDocument)
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
	// envconsul invocation would look for a token -- must have no
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
// that was never exercised. This one writes to /tmp (which must succeed,
// because the Go toolchain writes to os.TempDir() on every build) and then to
// the root filesystem (which must fail). Without the writable /tmp the
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
	request := liveRequest("oberth-live-rootfs", periapsis.TriggerCI, liveFilesystemDocument)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
		defer cleanupCancel()
		_ = controller.Cancel(cleanupCtx, request.Name, request.RunID)
	})
	if _, err := controller.Create(ctx, request); err != nil {
		t.Fatalf("create: %v", err)
	}
	log := &recordingWriter{}
	completion, err := controller.Wait(ctx, request.Name, request.RunID, log)
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
	request := liveRequest("oberth-live-success", periapsis.TriggerCI, liveTwoBurnDocument)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
		defer cleanupCancel()
		_ = controller.Cancel(cleanupCtx, request.Name, request.RunID)
	})
	if _, err := controller.Create(ctx, request); err != nil {
		t.Fatalf("create: %v", err)
	}

	log := &recordingWriter{}
	completion, err := controller.Wait(ctx, request.Name, request.RunID, log)
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
	burns := map[string][]string{}
	for _, step := range completion.Steps {
		burns[step.Burn] = append(burns[step.Burn], step.Step)
		if step.Status != runprogress.StepPassed {
			t.Errorf("step %s/%s = %s", step.Burn, step.Step, step.Status)
		}
	}
	for _, burn := range []string{"setup", "verify"} {
		if len(burns[burn]) == 0 {
			t.Errorf("no steps attributed to burn %q; observed %v", burn, burns)
		}
	}

	body := log.String()
	// Progress markers drive the dashboard and the step index.
	if !strings.Contains(body, runprogress.MarkerPrefix) {
		t.Error("no step progress markers were written")
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
	request := liveRequest("oberth-live-failure", periapsis.TriggerCI, liveFailingDocument)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
		defer cleanupCancel()
		_ = controller.Cancel(cleanupCtx, request.Name, request.RunID)
	})
	if _, err := controller.Create(ctx, request); err != nil {
		t.Fatalf("create: %v", err)
	}
	completion, err := controller.Wait(ctx, request.Name, request.RunID, &recordingWriter{})
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
	request := liveRequest("oberth-live-recovery", periapsis.TriggerCI, liveTwoBurnDocument)
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
		defer cleanupCancel()
		_ = controller.Cancel(cleanupCtx, request.Name, request.RunID)
	})
	if _, err := controller.Create(ctx, request); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := controller.Wait(ctx, request.Name, request.RunID, &recordingWriter{}); err != nil {
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
	request := liveRequest("oberth-live-idempotent", periapsis.TriggerCI, liveTwoBurnDocument)
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

func liveRequest(name string, trigger periapsis.Trigger, source string) Request {
	return Request{
		RunID: name, Name: name, Repo: "oberth", UpstreamOrg: "skipops",
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
const liveTwoBurnDocument = `
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
// real step: /tmp must be writable, the root filesystem must not be.
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
