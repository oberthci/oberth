package argojob

import (
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes/fake"
	kexec "k8s.io/client-go/util/exec"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/pipeline"
)

// builtStepPodLabels are the labels Build stamps on every step Pod of an
// assisted branch-CI run (request.Trigger = pipeline.TriggerCI). Fixtures
// take their labels from here, never from a literal, so the sweep selector
// and the spec builder cannot drift apart again.
func builtStepPodLabels(workflow string) map[string]string {
	built, err := Build(assistConfig(), assistRequest(pipeline.TriggerCI, assistDocument))
	if err != nil {
		panic(err)
	}
	labels := map[string]string{argoWorkflowLabel: workflow}
	for key, value := range built.Spec.PodMetadata.Labels {
		labels[key] = value
	}
	return labels
}

func heldPodFixture(name, workflow, step string, startedAgo time.Duration, helper bool) *corev1.Pod {
	main := corev1.Container{Name: mainContainerName, Image: "golang", Command: []string{"go", "test"},
		Env: []corev1.EnvVar{{Name: assistPauseEnv, Value: "true"}, {Name: AssistHoldEnv, Value: "1"}, {Name: "OBERTH_STEP", Value: step}}}
	if helper {
		main.VolumeMounts = []corev1.VolumeMount{{Name: SourceVolumeName, MountPath: assistBinMountPath, SubPath: "bin", ReadOnly: true}}
	}
	deadline := int64(2100)
	started := metav1.NewTime(time.Now().Add(-startedAgo))
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace,
			Labels:      builtStepPodLabels(workflow),
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
	if len(seen) < 4 || seen[0] != assistBinPath || seen[1] != "internal-hold-fs" || seen[2] != "release" || seen[4] != AssistReleasePath {
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

// failedAttemptFixture is attempt 0 of a held step: its main container
// exited with code.
func failedAttemptFixture(held *corev1.Pod, code int32) *corev1.Pod {
	pod := held.DeepCopy()
	pod.Name = strings.TrimSuffix(held.Name, "-1") + "-0"
	pod.Annotations = map[string]string{argoNodeNameAnnotation: strings.TrimSuffix(held.Annotations[argoNodeNameAnnotation], "(1)") + "(0)"}
	pod.Status.Phase = corev1.PodFailed
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: mainContainerName, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: code}}}}
	return pod
}

func assistSweepController(t *testing.T, client *fake.Clientset) *Controller {
	t.Helper()
	config := testConfig()
	config.Assist = AssistConfig{Enabled: true, MaxConcurrentHolds: 2, HoldTTL: 20 * time.Minute}
	controller, err := NewController(&staticWorkflowClient{}, client, config)
	if err != nil {
		t.Fatal(err)
	}
	return controller
}

func abandonedPods(abandoned []model.AssistAbandon) []string {
	var pods []string
	for _, abandon := range abandoned {
		pods = append(pods, abandon.Pod+"/"+abandon.Reason)
	}
	return pods
}

// TestEnforceAssistHoldsFindsPodsLabelledByBuild is the v0.18.1 regression:
// the sweep selected oberth.ci/trigger=branch while Build labels step Pods
// with pipeline.TriggerCI, so it never saw a hold. A Pod carrying exactly
// Build's labels must be found, aged and abandoned with its run, step and
// attempt-0 exit code.
func TestEnforceAssistHoldsFindsPodsLabelledByBuild(t *testing.T) {
	built, err := Build(assistConfig(), assistRequest(pipeline.TriggerCI, assistDocument))
	if err != nil {
		t.Fatal(err)
	}
	selector, err := labels.Parse(assistSweepSelector)
	if err != nil {
		t.Fatal(err)
	}
	if !selector.Matches(labels.Set(built.Spec.PodMetadata.Labels)) {
		t.Fatalf("sweep selector %q does not match Build's step Pod labels %v", assistSweepSelector, built.Spec.PodMetadata.Labels)
	}
	held := heldPodFixture("wf-a-test-1", "wf-a", "test", 21*time.Minute, false)
	client := fake.NewClientset(held, failedAttemptFixture(held, 3))
	controller := assistSweepController(t, client)
	now := time.Now()
	abandoned, err := controller.EnforceAssistHolds(context.Background(), nil, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(abandoned) != 1 {
		t.Fatalf("abandoned = %+v, want the expired hold", abandoned)
	}
	got := abandoned[0]
	if got.Pod != "wf-a-test-1" || got.Step != "test" || got.Reason != model.AssistAbandonTTL || got.RunID != labelValue(testRequest(pipeline.TriggerCI, "").RunID) ||
		got.FailedAttempt != 0 || got.FailedExitCode == nil || *got.FailedExitCode != 3 || !got.AbandonedAt.Equal(now) || got.HeldSince.IsZero() {
		t.Fatalf("abandon = %+v", got)
	}
	if _, err := client.CoreV1().Pods(testNamespace).Get(context.Background(), "wf-a-test-1", metav1.GetOptions{}); err == nil {
		t.Fatal("expired hold Pod was not deleted")
	}
	// A release-tier Pod is outside the selector: it can never be a hold.
	release := heldPodFixture("wf-r-test-1", "wf-r", "test", time.Hour, false)
	release.Labels[triggerLabel] = string(pipeline.TriggerRelease)
	client = fake.NewClientset(release)
	if abandoned, err := assistSweepController(t, client).EnforceAssistHolds(context.Background(), nil, nil, now); err != nil || len(abandoned) != 0 {
		t.Fatalf("release Pod swept: %v %v", abandoned, err)
	}
}

// TestEnforceAssistHoldsCapRefusesNewestIdleHold pins the cap rule: above
// assist.maxConcurrentHolds the newest idle hold is abandoned (an over-cap
// hold fails at the first sweep after it parks, as if unassisted); a hold
// with agent activity inside model.AssistActivityWindow is never chosen,
// even when it is the newest; released holds are neither aged nor counted.
func TestEnforceAssistHoldsCapRefusesNewestIdleHold(t *testing.T) {
	now := time.Now()
	oldest := heldPodFixture("wf-a-test-1", "wf-a", "test", 10*time.Minute, false)
	middle := heldPodFixture("wf-b-test-1", "wf-b", "test", 5*time.Minute, false)
	newest := heldPodFixture("wf-c-test-1", "wf-c", "test", time.Minute, false)
	released := heldPodFixture("wf-d-test-1", "wf-d", "test", 40*time.Minute, false)
	expired := heldPodFixture("wf-e-test-1", "wf-e", "test", 25*time.Minute, false)
	client := fake.NewClientset(oldest, middle, newest, released, expired)
	controller := assistSweepController(t, client)
	activity := map[string]model.AssistPodActivity{"wf-d-test-1": {Released: true}}
	abandoned, err := controller.EnforceAssistHolds(context.Background(), activity, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(abandonedPods(abandoned), ","); got != "wf-e-test-1/ttl,wf-c-test-1/cap" {
		t.Fatalf("abandoned = %s, want the expired hold and the newest idle hold above the cap", got)
	}
	if _, err := client.CoreV1().Pods(testNamespace).Get(context.Background(), "wf-d-test-1", metav1.GetOptions{}); err != nil {
		t.Fatal("a released hold past the TTL was abandoned: its command is running")
	}

	// The newest hold is being worked (a patch two minutes ago) and the
	// middle one saw activity long ago: the idle middle hold goes, not the
	// worked one.
	oldest = heldPodFixture("wf-a-test-1", "wf-a", "test", 10*time.Minute, false)
	middle = heldPodFixture("wf-b-test-1", "wf-b", "test", 9*time.Minute, false)
	newest = heldPodFixture("wf-c-test-1", "wf-c", "test", 3*time.Minute, false)
	client = fake.NewClientset(oldest, middle, newest)
	activity = map[string]model.AssistPodActivity{
		"wf-c-test-1": {LastAction: now.Add(-2 * time.Minute)},
		"wf-b-test-1": {LastAction: now.Add(-model.AssistActivityWindow - time.Second)},
	}
	abandoned, err = assistSweepController(t, client).EnforceAssistHolds(context.Background(), activity, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(abandonedPods(abandoned), ","); got != "wf-b-test-1/cap" {
		t.Fatalf("abandoned = %s, want the newest idle hold, never the worked one", got)
	}

	// Every hold above the cap is being worked: the cap abandons nothing.
	client = fake.NewClientset(heldPodFixture("wf-a-test-1", "wf-a", "test", 3*time.Minute, false),
		heldPodFixture("wf-b-test-1", "wf-b", "test", 2*time.Minute, false), heldPodFixture("wf-c-test-1", "wf-c", "test", time.Minute, false))
	recent := model.AssistPodActivity{LastAction: now.Add(-time.Minute)}
	activity = map[string]model.AssistPodActivity{"wf-a-test-1": recent, "wf-b-test-1": recent, "wf-c-test-1": recent}
	if abandoned, err := assistSweepController(t, client).EnforceAssistHolds(context.Background(), activity, nil, now); err != nil || len(abandoned) != 0 {
		t.Fatalf("cap abandoned a worked hold: %v %v", abandonedPods(abandoned), err)
	}

	config := testConfig()
	config.Assist = AssistConfig{Enabled: false}
	disabled, _ := NewController(&staticWorkflowClient{}, client, config)
	if abandoned, err := disabled.EnforceAssistHolds(context.Background(), nil, nil, now); err != nil || abandoned != nil {
		t.Fatalf("disabled policy acted: %v %v", abandoned, err)
	}
}

// TestAssistHoldStepWithoutStepEnvMatchesFailedStep: a held Pod without
// OBERTH_STEP takes its step from the Argo node name by the failure
// display's rule (the node's display name without the retry suffix), never
// the raw node name, so the sweep record matches the run's FailedStep.
func TestAssistHoldStepWithoutStepEnvMatchesFailedStep(t *testing.T) {
	for node, want := range map[string]string{"wf-a.test(1)": "test", "wf-a.ci.test(1)": "test", "wf-a.ci.unit-tests(2)": "unit-tests"} {
		held := heldPodFixture("wf-a-test-1", "wf-a", "test", 21*time.Minute, false)
		held.Spec.Containers[0].Env = []corev1.EnvVar{{Name: assistPauseEnv, Value: "true"}, {Name: AssistHoldEnv, Value: "1"}}
		held.Annotations[argoNodeNameAnnotation] = node
		hold, ok := assistHoldOf(held)
		if !ok || hold.Step != want {
			t.Fatalf("node %s: hold = %+v %v, want step %s", node, hold, ok, want)
		}
		abandoned, err := assistSweepController(t, fake.NewClientset(held)).EnforceAssistHolds(context.Background(), nil, nil, time.Now())
		if err != nil || len(abandoned) != 1 {
			t.Fatalf("node %s: abandoned = %+v %v", node, abandoned, err)
		}
		record := model.RunAssistRecord{Events: []model.RunAssistEvent{abandoned[0].Event()}}
		if !record.SweepAbandoned(want) {
			t.Fatalf("node %s: sweep record step %q does not match FailedStep %q", node, abandoned[0].Step, want)
		}
	}
}

// TestEnforceAssistHoldsRechecksVictimBeforeDelete is the snapshot race: the
// activity snapshot predates the Pod list, so a continue (or, for the cap, an
// exec or patch) landing in between must spare the victim. The sweep re-reads
// each victim right before the delete; an unreadable record spares it too,
// and the cap then moves on to the next idle hold.
func TestEnforceAssistHoldsRechecksVictimBeforeDelete(t *testing.T) {
	now := time.Now()
	expired := heldPodFixture("wf-a-test-1", "wf-a", "test", 21*time.Minute, false)
	client := fake.NewClientset(expired)
	continued := func(_ context.Context, _, pod string) (model.AssistPodActivity, error) {
		return model.AssistPodActivity{Released: true, LastAction: now}, nil
	}
	abandoned, err := assistSweepController(t, client).EnforceAssistHolds(context.Background(), nil, continued, now)
	if err != nil || len(abandoned) != 0 {
		t.Fatalf("a hold continued after the snapshot was abandoned: %v %v", abandonedPods(abandoned), err)
	}
	if _, err := client.CoreV1().Pods(testNamespace).Get(context.Background(), "wf-a-test-1", metav1.GetOptions{}); err != nil {
		t.Fatal("continued hold Pod was deleted")
	}
	// Cap 2: the expired hold continued after the snapshot is running work,
	// so with two idle holds beside it nothing is over the cap.
	client = fake.NewClientset(heldPodFixture("wf-a-test-1", "wf-a", "test", 21*time.Minute, false),
		heldPodFixture("wf-b-test-1", "wf-b", "test", 2*time.Minute, false), heldPodFixture("wf-c-test-1", "wf-c", "test", time.Minute, false))
	onlyExpiredContinued := func(_ context.Context, _, pod string) (model.AssistPodActivity, error) {
		return model.AssistPodActivity{Released: pod == "wf-a-test-1"}, nil
	}
	if abandoned, err := assistSweepController(t, client).EnforceAssistHolds(context.Background(), nil, onlyExpiredContinued, now); err != nil || len(abandoned) != 0 {
		t.Fatalf("a hold released after the snapshot still counted toward the cap: %v %v", abandonedPods(abandoned), err)
	}
	client = fake.NewClientset(expired)
	unreadable := func(context.Context, string, string) (model.AssistPodActivity, error) {
		return model.AssistPodActivity{}, errors.New("database locked")
	}
	if abandoned, err := assistSweepController(t, client).EnforceAssistHolds(context.Background(), nil, unreadable, now); err != nil || len(abandoned) != 0 {
		t.Fatalf("an unreadable record did not spare the victim: %v %v", abandonedPods(abandoned), err)
	}

	// Cap 2, three idle holds in the snapshot: the newest is the victim, but
	// its agent patched it after the snapshot. It is spared and the next idle
	// hold goes instead.
	client = fake.NewClientset(heldPodFixture("wf-a-test-1", "wf-a", "test", 3*time.Minute, false),
		heldPodFixture("wf-b-test-1", "wf-b", "test", 2*time.Minute, false), heldPodFixture("wf-c-test-1", "wf-c", "test", time.Minute, false))
	var rechecked []string
	patched := func(_ context.Context, _, pod string) (model.AssistPodActivity, error) {
		rechecked = append(rechecked, pod)
		if pod == "wf-c-test-1" {
			return model.AssistPodActivity{LastAction: now.Add(-time.Second)}, nil
		}
		return model.AssistPodActivity{}, nil
	}
	abandoned, err = assistSweepController(t, client).EnforceAssistHolds(context.Background(), map[string]model.AssistPodActivity{}, patched, now)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(abandonedPods(abandoned), ","); got != "wf-b-test-1/cap" || strings.Join(rechecked, ",") != "wf-c-test-1,wf-b-test-1" {
		t.Fatalf("abandoned = %s after rechecking %v, want the patched newest hold spared and the next idle one abandoned", got, rechecked)
	}
	if _, err := client.CoreV1().Pods(testNamespace).Get(context.Background(), "wf-c-test-1", metav1.GetOptions{}); err != nil {
		t.Fatal("hold patched after the snapshot was deleted")
	}
}

// TestEnforceAssistHoldsTTLExtendsOnActivity: an idle hold is still
// abandoned at held_since + holdTTL; a hold with an agent action ages from
// that action instead, but never past AssistHoldMaxAgeFactor TTLs after
// held_since, which the held Pod's deadline (budget + 2 TTLs) covers.
func TestEnforceAssistHoldsTTLExtendsOnActivity(t *testing.T) {
	ttl := 20 * time.Minute
	idle := heldPodFixture("wf-a-test-1", "wf-a", "test", ttl, false)
	worked := heldPodFixture("wf-b-test-1", "wf-b", "test", 25*time.Minute, false)
	stale := heldPodFixture("wf-c-test-1", "wf-c", "test", 30*time.Minute, false)
	capped := heldPodFixture("wf-d-test-1", "wf-d", "test", AssistHoldMaxAgeFactor*ttl, false)
	now := time.Now() // after the fixtures: idle and capped are exactly at their bound
	client := fake.NewClientset(idle, worked, stale, capped)
	activity := map[string]model.AssistPodActivity{
		"wf-b-test-1": {LastAction: now.Add(-10 * time.Minute)}, // extended to 30m: still held
		"wf-c-test-1": {LastAction: now.Add(-ttl)},              // its last action is one TTL old
		"wf-d-test-1": {LastAction: now.Add(-time.Second)},      // worked, but at the hard cap
	}
	config := testConfig()
	config.Assist = AssistConfig{Enabled: true, MaxConcurrentHolds: 64, HoldTTL: ttl}
	controller, err := NewController(&staticWorkflowClient{}, client, config)
	if err != nil {
		t.Fatal(err)
	}
	abandoned, err := controller.EnforceAssistHolds(context.Background(), activity, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, pod := range abandonedPods(abandoned) {
		got[pod] = true
	}
	if len(got) != 3 || !got["wf-a-test-1/ttl"] || !got["wf-c-test-1/ttl"] || !got["wf-d-test-1/ttl"] {
		t.Fatalf("abandoned = %v, want the idle hold at TTL, the stale worked hold and the capped one", abandonedPods(abandoned))
	}
	if _, err := client.CoreV1().Pods(testNamespace).Get(context.Background(), "wf-b-test-1", metav1.GetOptions{}); err != nil {
		t.Fatal("a hold worked 10 minutes ago was abandoned at its original TTL")
	}

	// A patch landing after the snapshot extends a TTL victim on recheck.
	client = fake.NewClientset(heldPodFixture("wf-e-test-1", "wf-e", "test", 21*time.Minute, false))
	patched := func(context.Context, string, string) (model.AssistPodActivity, error) {
		return model.AssistPodActivity{LastAction: now.Add(-time.Second)}, nil
	}
	if abandoned, err := assistSweepController(t, client).EnforceAssistHolds(context.Background(), nil, patched, now); err != nil || len(abandoned) != 0 {
		t.Fatalf("a hold patched after the snapshot was abandoned at its original TTL: %v %v", abandonedPods(abandoned), err)
	}

	// The held Pod's deadline covers the cap plus the whole step budget.
	built, err := Build(assistConfig(), assistRequest(pipeline.TriggerCI, assistDocument))
	if err != nil {
		t.Fatal(err)
	}
	ttlSeconds := int64(assistConfig().Assist.Effective().HoldTTL / time.Second)
	budget := int64(600)
	if got := templateNamed(t, built, "test").Metadata.Annotations[AssistHoldAnnotation]; got != strconv.FormatInt(budget+AssistHoldMaxAgeFactor*ttlSeconds, 10) {
		t.Fatalf("held Pod deadline = %s, want budget %d + %d TTLs of %ds", got, budget, AssistHoldMaxAgeFactor, ttlSeconds)
	}
}
