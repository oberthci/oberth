package argojob

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	"github.com/oberthci/oberth/pkg/periapsis"
)

func terminalControllerHistory(t *testing.T) []corev1.Pod {
	t.Helper()
	data, err := os.ReadFile("testdata/controller-terminal-history.json")
	if err != nil {
		t.Fatal(err)
	}
	var pods []corev1.Pod
	if err := json.Unmarshal(data, &pods); err != nil {
		t.Fatal(err)
	}
	if len(pods) != 2 {
		t.Fatal("expected two independently observed historical records")
	}
	return pods
}

func TestNonrootTerminalHistoryRejectsUncertainIncarnations(t *testing.T) {
	cases := map[string]func(*corev1.Pod){
		"namespace":              func(p *corev1.Pod) { p.Namespace = "foreign" },
		"missing-uid":            func(p *corev1.Pod) { p.UID = "" },
		"duplicate-current-uid":  func(p *corev1.Pod) { p.UID = "pod-uid" },
		"missing-name":           func(p *corev1.Pod) { p.Name = "" },
		"duplicate-current-name": func(p *corev1.Pod) { p.Name = "controller-fixture" },
		"missing-version":        func(p *corev1.Pod) { p.ResourceVersion = "" },
		"missing-created":        func(p *corev1.Pod) { p.CreationTimestamp = metav1.Time{} },
		"future-created": func(p *corev1.Pod) {
			p.CreationTimestamp = metav1.NewTime(time.Now().Add(time.Hour))
			s := p.Status.ContainerStatuses[0].State.Terminated
			s.StartedAt = metav1.NewTime(p.CreationTimestamp.Add(time.Second))
			s.FinishedAt = metav1.NewTime(p.CreationTimestamp.Add(2 * time.Second))
		},
		"deleting":            func(p *corev1.Pod) { p.DeletionTimestamp = ptr.To(metav1.Now()) },
		"pending":             func(p *corev1.Pod) { p.Status.Phase = corev1.PodPending },
		"unknown":             func(p *corev1.Pod) { p.Status.Phase = corev1.PodUnknown },
		"no-phase":            func(p *corev1.Pod) { p.Status.Phase = "" },
		"no-owner":            func(p *corev1.Pod) { p.OwnerReferences = nil },
		"foreign-owner":       func(p *corev1.Pod) { p.OwnerReferences[0].Kind = "Job" },
		"no-owner-uid":        func(p *corev1.Pod) { p.OwnerReferences[0].UID = "" },
		"uncontrolled-owner":  func(p *corev1.Pod) { p.OwnerReferences[0].Controller = ptr.To(false) },
		"extra-owner":         func(p *corev1.Pod) { p.OwnerReferences = append(p.OwnerReferences, p.OwnerReferences[0]) },
		"extra-container":     func(p *corev1.Pod) { p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: "extra"}) },
		"wrong-container":     func(p *corev1.Pod) { p.Spec.Containers[0].Name = "other" },
		"init-container":      func(p *corev1.Pod) { p.Spec.InitContainers = []corev1.Container{{Name: "init"}} },
		"ephemeral-container": func(p *corev1.Pod) { p.Spec.EphemeralContainers = []corev1.EphemeralContainer{{}} },
		"init-status":         func(p *corev1.Pod) { p.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "init"}} },
		"ephemeral-status":    func(p *corev1.Pod) { p.Status.EphemeralContainerStatuses = []corev1.ContainerStatus{{Name: "debug"}} },
		"missing-status":      func(p *corev1.Pod) { p.Status.ContainerStatuses = nil },
		"duplicate-status": func(p *corev1.Pod) {
			p.Status.ContainerStatuses = append(p.Status.ContainerStatuses, p.Status.ContainerStatuses[0])
		},
		"wrong-status-name": func(p *corev1.Pod) { p.Status.ContainerStatuses[0].Name = "other" },
		"container-ready":   func(p *corev1.Pod) { p.Status.ContainerStatuses[0].Ready = true },
		"container-started": func(p *corev1.Pod) { p.Status.ContainerStatuses[0].Started = ptr.To(true) },
		"started-unknown":   func(p *corev1.Pod) { p.Status.ContainerStatuses[0].Started = nil },
		"no-conditions":     func(p *corev1.Pod) { p.Status.Conditions = nil },
		"ready-true": func(p *corev1.Pod) {
			p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}, {Type: corev1.ContainersReady, Status: corev1.ConditionFalse}}
		},
		"ready-unknown": func(p *corev1.Pod) {
			p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionUnknown}, {Type: corev1.ContainersReady, Status: corev1.ConditionFalse}}
		},
		"duplicate-condition": func(p *corev1.Pod) { p.Status.Conditions = append(p.Status.Conditions, p.Status.Conditions[0]) },
		"missing-image-id":    func(p *corev1.Pod) { p.Status.ContainerStatuses[0].ImageID = "" },
		"missing-id":          func(p *corev1.Pod) { p.Status.ContainerStatuses[0].ContainerID = "" },
		"different-id":        func(p *corev1.Pod) { p.Status.ContainerStatuses[0].ContainerID = "containerd://foreign" },
		"empty-runtime-id": func(p *corev1.Pod) {
			s := &p.Status.ContainerStatuses[0]
			s.ContainerID = "containerd://"
			s.State.Terminated.ContainerID = s.ContainerID
		},
		"missing-termination":      func(p *corev1.Pod) { p.Status.ContainerStatuses[0].State.Terminated = nil },
		"also-running":             func(p *corev1.Pod) { p.Status.ContainerStatuses[0].State.Running = &corev1.ContainerStateRunning{} },
		"also-waiting":             func(p *corev1.Pod) { p.Status.ContainerStatuses[0].State.Waiting = &corev1.ContainerStateWaiting{} },
		"unknown-current-reason":   func(p *corev1.Pod) { p.Status.ContainerStatuses[0].State.Terminated.Reason = "Unknown" },
		"synthetic-current-reason": func(p *corev1.Pod) { p.Status.ContainerStatuses[0].State.Terminated.Reason = "ContainerStatusUnknown" },
		"wrong-phase-exit":         func(p *corev1.Pod) { p.Status.Phase = corev1.PodSucceeded },
		"zero-failed-exit":         func(p *corev1.Pod) { p.Status.ContainerStatuses[0].State.Terminated.ExitCode = 0 },
		"missing-start":            func(p *corev1.Pod) { p.Status.ContainerStatuses[0].State.Terminated.StartedAt = metav1.Time{} },
		"missing-finish":           func(p *corev1.Pod) { p.Status.ContainerStatuses[0].State.Terminated.FinishedAt = metav1.Time{} },
		"future-finish": func(p *corev1.Pod) {
			p.Status.ContainerStatuses[0].State.Terminated.FinishedAt = metav1.NewTime(time.Now().Add(time.Hour))
		},
		"finish-before-start": func(p *corev1.Pod) {
			s := p.Status.ContainerStatuses[0].State.Terminated
			s.FinishedAt = metav1.NewTime(s.StartedAt.Add(-time.Second))
		},
		"start-before-pod": func(p *corev1.Pod) {
			p.Status.ContainerStatuses[0].State.Terminated.StartedAt = metav1.NewTime(p.CreationTimestamp.Add(-time.Second))
		},
		"negative-restarts":       func(p *corev1.Pod) { p.Status.ContainerStatuses[0].RestartCount = -1 },
		"restart-without-history": func(p *corev1.Pod) { p.Status.ContainerStatuses[0].RestartCount = 1 },
		"history-without-restart": func(p *corev1.Pod) { s := &p.Status.ContainerStatuses[0]; s.LastTerminationState = *s.State.DeepCopy() },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			current, config := nonrootVerifierObjects()
			history := terminalControllerHistory(t)[0]
			mutate(&history)
			// Use the selector directly for malformed API identities; fake API
			// filtering must not make a wrong-namespace record silently absent.
			if _, err := selectNonrootController([]corev1.Pod{*current, history}, testNamespace); err == nil {
				t.Fatal("uncertain lifecycle removed from controller ambiguity")
			}
			client := fake.NewClientset(current, config)
			client.PrependReactor("list", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
				return true, &corev1.PodList{Items: []corev1.Pod{*current, history}}, nil
			})
			_, err := verifyNonrootController(t.Context(), client, testNamespace, testRequest(periapsis.TriggerCI, nonrootTestDocument))
			if err == nil {
				t.Fatal("full profile accepted uncertain historical record")
			}
		})
	}
}

func TestNonrootTerminalObservationTimeBoundsBothIncarnations(t *testing.T) {
	p := terminalControllerHistory(t)[1]
	observed := p.Status.ContainerStatuses[0].State.Terminated.FinishedAt.Time
	if !nonrootControllerHistoryIsTerminal(&p, observed) {
		t.Fatal("termination at the captured observation boundary refused")
	}
	if nonrootControllerHistoryIsTerminal(&p, observed.Add(-time.Second)) {
		t.Fatal("future final incarnation accepted")
	}
	prior := p.Status.ContainerStatuses[0].LastTerminationState
	if nonrootTerminatedIncarnation(prior, prior.Terminated.FinishedAt.Add(-time.Second)) {
		t.Fatal("future previous incarnation accepted")
	}
	prior.Terminated.StartedAt = metav1.NewTime(observed.Add(time.Second))
	prior.Terminated.FinishedAt = metav1.NewTime(observed.Add(2 * time.Second))
	if nonrootTerminatedIncarnation(prior, observed) {
		t.Fatal("ordered but wholly future previous incarnation accepted")
	}
}

func TestNonrootTerminalPriorIncarnationAndLiveAmbiguity(t *testing.T) {
	for _, mutate := range []func(*corev1.ContainerStatus){
		func(s *corev1.ContainerStatus) { s.LastTerminationState = corev1.ContainerState{} },
		func(s *corev1.ContainerStatus) { s.LastTerminationState.Running = &corev1.ContainerStateRunning{} },
		func(s *corev1.ContainerStatus) { s.LastTerminationState.Terminated.ContainerID = s.ContainerID },
		func(s *corev1.ContainerStatus) {
			s.LastTerminationState.Terminated.FinishedAt = metav1.NewTime(s.State.Terminated.StartedAt.Add(time.Second))
		},
		func(s *corev1.ContainerStatus) { s.LastTerminationState.Terminated.StartedAt = metav1.Time{} },
	} {
		p := terminalControllerHistory(t)[1]
		mutate(&p.Status.ContainerStatuses[0])
		if nonrootControllerHistoryIsTerminal(&p, time.Now()) {
			t.Fatal("contradictory previous incarnation accepted")
		}
	}
	current, _ := nonrootVerifierObjects()
	other := current.DeepCopy()
	other.UID = "second-live-controller"
	other.Name = "second-live-controller"
	if _, err := selectNonrootController([]corev1.Pod{*current, *other}, testNamespace); err == nil {
		t.Fatal("two live controllers accepted")
	}
	if _, err := selectNonrootController(terminalControllerHistory(t), testNamespace); err == nil {
		t.Fatal("terminal history substituted for live controller")
	}
	for _, phase := range []corev1.PodPhase{"", corev1.PodPending, corev1.PodUnknown} {
		current.Status.Phase = phase
		if _, err := selectNonrootController([]corev1.Pod{*current}, testNamespace); err == nil {
			t.Fatal("ready flags overrode unknown/nonrunning Pod phase")
		}
	}
}

func TestNonrootTerminalHistoryNeverBypassesLiveProfile(t *testing.T) {
	current, config := nonrootVerifierObjects()
	current.Spec.Containers[0].Image = "public-wrong-image"
	history := terminalControllerHistory(t)
	_, err := verifyNonrootController(t.Context(), fake.NewClientset(current, config, &history[0], &history[1]), testNamespace, testRequest(periapsis.TriggerCI, nonrootTestDocument))
	if err == nil || !strings.Contains(err.Error(), "controller image or running image identity") {
		t.Fatal("terminal selection bypassed or did not reach existing live identity checks")
	}
	completed := history[0].DeepCopy()
	completed.Status.Phase = corev1.PodSucceeded
	completed.Status.ContainerStatuses[0].State.Terminated.Reason = "Completed"
	completed.Status.ContainerStatuses[0].State.Terminated.ExitCode = 0
	if !nonrootControllerHistoryIsTerminal(completed, time.Now()) {
		t.Fatal("fully substantiated successful terminal record refused")
	}
}

func TestNonrootControllerWithObservedTerminalHistory(t *testing.T) {
	current, config := nonrootVerifierObjects()
	objects := []runtime.Object{current, config}
	for _, pod := range terminalControllerHistory(t) {
		objects = append(objects, pod.DeepCopy())
	}
	proof, err := verifyNonrootController(t.Context(), fake.NewClientset(objects...), testNamespace, testRequest(periapsis.TriggerCI, nonrootTestDocument))
	if err != nil {
		t.Fatal(err)
	}
	if proof.podUID != string(current.UID) {
		t.Fatal("historical record substituted for current controller authority")
	}
}
