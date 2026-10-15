package installer

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

func TestControllerServerRetainedCompletedHistory(t *testing.T) {
	cfg, deps, _ := controllerUpgradeFixture(t)
	client := controllerProfileKube(t, "oberth-argo")
	deps.KubeClient = client
	dep := controllerServerFixture(t, client)
	raw, err := os.ReadFile("testdata/server-terminal-history.json")
	if err != nil {
		t.Fatal(err)
	}
	var old corev1.Pod
	if err = json.Unmarshal(raw, &old); err != nil {
		t.Fatal(err)
	}
	if _, err = client.CoreV1().Pods("oberth").Create(t.Context(), &old, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	pod, err := controllerServerPod(t.Context(), cfg, deps, dep)
	if err != nil || pod == nil || pod.Name != "oberth-pod" {
		t.Fatalf("complete retained server history blocked live candidate: %v", err)
	}
}

func serverHistoryFixture(t *testing.T) corev1.Pod {
	t.Helper()
	raw, e := os.ReadFile("testdata/server-terminal-history.json")
	if e != nil {
		t.Fatal(e)
	}
	var p corev1.Pod
	if e = json.Unmarshal(raw, &p); e != nil {
		t.Fatal(e)
	}
	return p
}
func TestUpgradeServerHistoryRejectsAmbiguousIncarnations(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	tests := map[string]func(*corev1.Pod){
		"future-creation": func(p *corev1.Pod) { p.CreationTimestamp = metav1.NewTime(now.Add(time.Second)) },
		"future-main-finish": func(p *corev1.Pod) {
			p.Status.ContainerStatuses[0].State.Terminated.FinishedAt = metav1.NewTime(now.Add(time.Second))
		},
		"future-init-finish": func(p *corev1.Pod) {
			p.Status.InitContainerStatuses[0].State.Terminated.FinishedAt = metav1.NewTime(now.Add(time.Second))
		},
		"deleting":        func(p *corev1.Pod) { p.DeletionTimestamp = &metav1.Time{Time: now} },
		"missing-version": func(p *corev1.Pod) { p.ResourceVersion = "" },
		"missing-owner":   func(p *corev1.Pod) { p.OwnerReferences = nil },
		"wrong-owner":     func(p *corev1.Pod) { p.OwnerReferences[0].Kind = "Job" },
		"ready":           func(p *corev1.Pod) { p.Status.ContainerStatuses[0].Ready = true },
		"started":         func(p *corev1.Pod) { p.Status.ContainerStatuses[0].Started = ptr.To(true) },
		"init-started":    func(p *corev1.Pod) { p.Status.InitContainerStatuses[0].Started = ptr.To(true) },
		"missing-started": func(p *corev1.Pod) { p.Status.ContainerStatuses[0].Started = nil },
		"error-with-succeeded-phase": func(p *corev1.Pod) {
			p.Status.ContainerStatuses[0].State.Terminated.Reason = "Error"
			p.Status.ContainerStatuses[0].State.Terminated.ExitCode = 137
		},
		"completed-with-failed-phase": func(p *corev1.Pod) { p.Status.Phase = corev1.PodFailed },
		"zero-failed-exit": func(p *corev1.Pod) {
			p.Status.Phase = corev1.PodFailed
			p.Status.ContainerStatuses[0].State.Terminated.Reason = "OOMKilled"
		},
		"synthetic-current":   func(p *corev1.Pod) { p.Status.ContainerStatuses[0].State.Terminated.Reason = "ContainerStatusUnknown" },
		"synthetic-init":      func(p *corev1.Pod) { p.Status.InitContainerStatuses[0].State.Terminated.Reason = "Unknown" },
		"init-failed":         func(p *corev1.Pod) { p.Status.InitContainerStatuses[0].State.Terminated.ExitCode = 1 },
		"main-signal":         func(p *corev1.Pod) { p.Status.ContainerStatuses[0].State.Terminated.Signal = 9 },
		"init-running":        func(p *corev1.Pod) { p.Status.InitContainerStatuses[0].State.Running = &corev1.ContainerStateRunning{} },
		"main-waiting":        func(p *corev1.Pod) { p.Status.ContainerStatuses[0].State.Waiting = &corev1.ContainerStateWaiting{} },
		"missing-init-status": func(p *corev1.Pod) { p.Status.InitContainerStatuses = nil },
		"extra-init-status": func(p *corev1.Pod) {
			p.Status.InitContainerStatuses = append(p.Status.InitContainerStatuses, p.Status.InitContainerStatuses[0])
		},
		"init-sidecar": func(p *corev1.Pod) {
			p.Spec.InitContainers[0].RestartPolicy = ptr.To(corev1.ContainerRestartPolicyAlways)
		},
		"extra-container":   func(p *corev1.Pod) { p.Spec.Containers = append(p.Spec.Containers, p.Spec.Containers[0]) },
		"extra-init":        func(p *corev1.Pod) { p.Spec.InitContainers = append(p.Spec.InitContainers, p.Spec.InitContainers[0]) },
		"ephemeral":         func(p *corev1.Pod) { p.Spec.EphemeralContainers = []corev1.EphemeralContainer{{}} },
		"ephemeral-status":  func(p *corev1.Pod) { p.Status.EphemeralContainerStatuses = []corev1.ContainerStatus{{}} },
		"wrong-init-name":   func(p *corev1.Pod) { p.Spec.InitContainers[0].Name = "foreign" },
		"wrong-status-name": func(p *corev1.Pod) { p.Status.InitContainerStatuses[0].Name = "foreign" },
		"wrong-image":       func(p *corev1.Pod) { p.Spec.InitContainers[0].Image = "foreign" },
		"reused-init-main-cid": func(p *corev1.Pod) {
			p.Status.InitContainerStatuses[0].ContainerID = p.Status.ContainerStatuses[0].ContainerID
			p.Status.InitContainerStatuses[0].State.Terminated.ContainerID = p.Status.ContainerStatuses[0].ContainerID
		},
		"wrong-cid": func(p *corev1.Pod) {
			p.Status.ContainerStatuses[0].ContainerID = "containerd://" + strings.Repeat("f", 64)
		},
		"malformed-cid": func(p *corev1.Pod) {
			p.Status.InitContainerStatuses[0].State.Terminated.ContainerID = "containerd://synthetic"
		},
		"missing-time": func(p *corev1.Pod) { p.Status.InitContainerStatuses[0].State.Terminated.StartedAt = metav1.Time{} },
		"init-finishes-after-main-start": func(p *corev1.Pod) {
			p.Status.InitContainerStatuses[0].State.Terminated.FinishedAt = p.Status.ContainerStatuses[0].State.Terminated.FinishedAt
		},
		"duplicate-condition": func(p *corev1.Pod) { p.Status.Conditions = append(p.Status.Conditions, p.Status.Conditions[0]) },
		"ready-condition": func(p *corev1.Pod) {
			for i := range p.Status.Conditions {
				if p.Status.Conditions[i].Type == corev1.PodReady {
					p.Status.Conditions[i].Status = corev1.ConditionTrue
				}
			}
		},
		"unproven-restart": func(p *corev1.Pod) { p.Status.ContainerStatuses[0].RestartCount = 1 },
		"last-without-restart": func(p *corev1.Pod) {
			p.Status.ContainerStatuses[0].LastTerminationState = p.Status.ContainerStatuses[0].State
		},
	}
	if !upgradeServerHistoryTerminal(ptr.To(serverHistoryFixture(t)), now) {
		t.Fatal("actual completed history refused")
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			p := serverHistoryFixture(t)
			mutate(&p)
			if upgradeServerHistoryTerminal(&p, now) {
				t.Fatal("ambiguous history accepted")
			}
		})
	}
}
func TestUpgradeServerSelectorRequiresOneLiveUniqueIdentity(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	old := serverHistoryFixture(t)
	live := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "live", Namespace: "oberth", UID: "live-uid"}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	if p, e := selectUpgradeServer([]corev1.Pod{old, live}, "oberth", now); e != nil || p.Name != "live" {
		t.Fatal("one live plus proven history refused")
	}
	for _, mode := range []string{"none", "two-live", "duplicate-name", "duplicate-uid", "foreign-namespace", "empty-name", "unknown", "deleting"} {
		t.Run(mode, func(t *testing.T) {
			p := live
			rows := []corev1.Pod{old, p}
			switch mode {
			case "none":
				rows = rows[:1]
			case "two-live":
				p.Name = "other"
				p.UID = "other"
				rows = append(rows, p)
			case "duplicate-name":
				rows[1].Name = old.Name
			case "duplicate-uid":
				rows[1].UID = old.UID
			case "foreign-namespace":
				rows[1].Namespace = "foreign"
			case "empty-name":
				rows[1].Name = ""
			case "unknown":
				rows[1].Status.Phase = corev1.PodUnknown
			case "deleting":
				rows[1].DeletionTimestamp = &metav1.Time{Time: now}
			}
			if _, e := selectUpgradeServer(rows, "oberth", now); e == nil {
				t.Fatal("ambiguous selection accepted")
			}
		})
	}
}

func TestUpgradeServerHistoryPriorIncarnationRemainsDistinct(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	for _, which := range []string{"ordinary", "init"} {
		t.Run(which, func(t *testing.T) {
			p := serverHistoryFixture(t)
			status := &p.Status.ContainerStatuses[0]
			if which == "init" {
				status = &p.Status.InitContainerStatuses[0]
			}
			prior := status.State.Terminated.DeepCopy()
			prior.ContainerID = "containerd://" + strings.Repeat("a", 64)
			prior.StartedAt = p.CreationTimestamp
			prior.FinishedAt = status.State.Terminated.StartedAt
			prior.Reason = "Unknown"
			prior.ExitCode = 137
			status.RestartCount = 1
			status.LastTerminationState = corev1.ContainerState{Terminated: prior}
			if !upgradeServerHistoryTerminal(&p, now) {
				t.Fatal("coherent prior incarnation refused")
			}
			prior.ContainerID = status.ContainerID
			if upgradeServerHistoryTerminal(&p, now) {
				t.Fatal("same incarnation reused")
			}
			prior.ContainerID = "containerd://" + strings.Repeat("a", 64)
			prior.FinishedAt = metav1.NewTime(status.State.Terminated.StartedAt.Add(time.Second))
			if upgradeServerHistoryTerminal(&p, now) {
				t.Fatal("overlapping incarnation accepted")
			}
		})
	}
	for _, reason := range []string{"Error", "OOMKilled"} {
		p := serverHistoryFixture(t)
		p.Status.Phase = corev1.PodFailed
		p.Status.ContainerStatuses[0].State.Terminated.Reason = reason
		p.Status.ContainerStatuses[0].State.Terminated.ExitCode = 137
		if !upgradeServerHistoryTerminal(&p, now) {
			t.Fatal("positive observed failed termination refused")
		}
	}
}
