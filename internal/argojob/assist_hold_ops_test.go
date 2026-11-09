package argojob

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	kexec "k8s.io/client-go/util/exec"

	"github.com/oberthci/oberth/internal/model"
)

func heldPodFixture(name, workflow, step string, startedAgo time.Duration, helper bool) *corev1.Pod {
	main := corev1.Container{Name: mainContainerName, Image: "golang", Command: []string{"go", "test"},
		Env: []corev1.EnvVar{{Name: assistPauseEnv, Value: "true"}, {Name: AssistHoldEnv, Value: "1"}, {Name: "OBERTH_STEP", Value: step}}}
	if helper {
		main.VolumeMounts = []corev1.VolumeMount{{Name: SourceVolumeName, MountPath: usageBinMountPath, SubPath: "bin", ReadOnly: true}}
	}
	deadline := int64(2100)
	started := metav1.NewTime(time.Now().Add(-startedAgo))
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace,
			Labels:      map[string]string{argoWorkflowLabel: workflow, "oberth.ci/trigger": "branch"},
			Annotations: map[string]string{AssistHoldAnnotation: "2100", argoNodeNameAnnotation: workflow + "." + step + "(1)"}},
		Spec: corev1.PodSpec{ActiveDeadlineSeconds: &deadline, Containers: []corev1.Container{main, {Name: "wait"}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, StartTime: &started,
			ContainerStatuses: []corev1.ContainerStatus{{Name: mainContainerName, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: started}}}}},
	}
}

func TestListAssistHoldsRecognisesParkedAttemptsOnly(t *testing.T) {
	held := heldPodFixture("wf-a-test-1", "wf-a", "test", time.Minute, true)
	first := heldPodFixture("wf-a-test-0", "wf-a", "test", 2*time.Minute, true)
	first.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "OBERTH_STEP", Value: "test"}} // attempt 1 has no pause
	finished := heldPodFixture("wf-a-lint-1", "wf-a", "lint", time.Minute, false)
	finished.Status.Phase = corev1.PodFailed
	// Attempt 1 carrying the pause through a repository env (refused at
	// admission, but defence in depth): no retry suffix, not a hold.
	spoofed := heldPodFixture("wf-a-build-0", "wf-a", "build", time.Minute, false)
	spoofed.Annotations[argoNodeNameAnnotation] = "wf-a.build"
	other := heldPodFixture("wf-b-test-1", "wf-b", "test", 3*time.Minute, false)
	client := fake.NewClientset(held, first, finished, other, spoofed)
	controller, err := NewController(&staticWorkflowClient{}, client, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	holds, err := controller.ListAssistHolds(context.Background(), "wf-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(holds) != 1 || holds[0].Pod != "wf-a-test-1" || holds[0].Step != "test" || holds[0].Attempt != 1 || !holds[0].PatchCapable || holds[0].SourcePath != SourceMountPath {
		t.Fatalf("holds = %+v", holds)
	}
	if remaining := time.Until(holds[0].Deadline); remaining < 2100*time.Second-2*time.Minute || remaining > 2100*time.Second {
		t.Fatalf("deadline not derived from the Pod: %s", remaining)
	}
	if err := controller.AbandonAssistHold(context.Background(), "wf-a-test-0"); !errors.Is(err, ErrNotHeld) {
		t.Fatalf("abandoning attempt 1 = %v, want not held", err)
	}
}

func TestExecAssistHoldBoundsOutputAndReportsExitCode(t *testing.T) {
	held := heldPodFixture("wf-a-test-1", "wf-a", "test", time.Minute, true)
	client := fake.NewClientset(held)
	var seen []string
	exec := func(_ context.Context, namespace, pod, container string, command []string, stdin io.Reader, stdout, stderr io.Writer) error {
		seen = command
		if namespace != testNamespace || pod != "wf-a-test-1" || container != mainContainerName {
			t.Fatalf("exec target %s/%s/%s", namespace, pod, container)
		}
		_, _ = io.WriteString(stdout, strings.Repeat("x", 100))
		_, _ = io.WriteString(stderr, "warn")
		if command[0] == "false" {
			return kexec.CodeExitError{Err: errors.New("exit"), Code: 3}
		}
		return nil
	}
	controller, err := NewController(&staticWorkflowClient{}, client, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	controller.WithSourceSeeder(NewSourceSeeder(client, exec, testConfig()))
	result, err := controller.ExecAssistHold(context.Background(), model.AssistExecRequest{Pod: "wf-a-test-1", Argv: []string{"false"}, OutputLimit: 10, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 3 || result.Stdout != strings.Repeat("x", 10) || !result.Truncated || result.StdoutBytes != 100 || result.Stderr != "warn" {
		t.Fatalf("result = %+v", result)
	}
	if err := controller.ReleaseAssistHold(context.Background(), "wf-a-test-1"); err != nil {
		t.Fatal(err)
	}
	if len(seen) < 4 || seen[0] != usageBinPath || seen[1] != "internal-hold-fs" || seen[2] != "release" || seen[4] != AssistReleasePath {
		t.Fatalf("release argv = %v", seen)
	}
	if _, err := controller.ExecAssistHold(context.Background(), model.AssistExecRequest{Pod: "missing", Argv: []string{"true"}}); err == nil {
		t.Fatal("exec into a missing pod succeeded")
	}
	if err := controller.AbandonAssistHold(context.Background(), "wf-a-test-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CoreV1().Pods(testNamespace).Get(context.Background(), "wf-a-test-1", metav1.GetOptions{}); err == nil {
		t.Fatal("abandon did not delete the pod")
	}
}

func TestEnforceAssistHoldsAbandonsExpiredAndAboveCap(t *testing.T) {
	oldest := heldPodFixture("wf-a-test-1", "wf-a", "test", 10*time.Minute, false)
	middle := heldPodFixture("wf-b-test-1", "wf-b", "test", 5*time.Minute, false)
	newest := heldPodFixture("wf-c-test-1", "wf-c", "test", time.Minute, false)
	released := heldPodFixture("wf-d-test-1", "wf-d", "test", 40*time.Minute, false)
	expired := heldPodFixture("wf-e-test-1", "wf-e", "test", 25*time.Minute, false)
	client := fake.NewClientset(oldest, middle, newest, released, expired)
	config := testConfig()
	config.Assist = AssistConfig{Enabled: true, MaxConcurrentHolds: 2, HoldTTL: 20 * time.Minute}
	controller, err := NewController(&staticWorkflowClient{}, client, config)
	if err != nil {
		t.Fatal(err)
	}
	abandoned, err := controller.EnforceAssistHolds(context.Background(), map[string]bool{"wf-d-test-1": true}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(abandoned) != 2 || abandoned[0] != "wf-e-test-1" || abandoned[1] != "wf-c-test-1" {
		t.Fatalf("abandoned = %v, want the expired hold and the newest above the cap", abandoned)
	}
	if _, err := client.CoreV1().Pods(testNamespace).Get(context.Background(), "wf-d-test-1", metav1.GetOptions{}); err != nil {
		t.Fatal("a released hold past the TTL was abandoned: its command is running")
	}
	config.Assist.Enabled = false
	disabled, _ := NewController(&staticWorkflowClient{}, client, config)
	if abandoned, err := disabled.EnforceAssistHolds(context.Background(), nil, time.Now()); err != nil || abandoned != nil {
		t.Fatalf("disabled policy acted: %v %v", abandoned, err)
	}
}
