package argojob

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runprogress"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

func restartCutFixture(t *testing.T, change func(*wfv1.Workflow, *wfv1.NodeStatus, *corev1.Pod)) (*Controller, *wfv1.Workflow, Adoption, []runprogress.Event) {
	t.Helper()
	wf, node, pod, _ := diagnosticFixture()
	wf.Annotations[identityAnnotation] = strings.Repeat("b", 64)
	wf.Annotations[refAnnotation] = "feature/restart"
	wf.Labels = map[string]string{"oberth.ci/sha": strings.Repeat("c", 40), "oberth.ci/trigger": "ci"}
	node.Phase = wfv1.NodeError
	node.Outputs = nil
	pod.Status.ContainerStatuses[0].State.Terminated = nil
	change(wf, &node, pod)
	wf.Status.Nodes[node.ID] = node
	kube := fake.NewClientset()
	if pod.Name != "" {
		if _, err := kube.CoreV1().Pods(pod.Namespace).Create(t.Context(), pod, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	controller := &Controller{restartedAt: time.Now().UTC(), workflows: &staticWorkflowClient{workflow: wf}, kube: kube, config: Config{Namespace: wf.Namespace, MaxRunLogBytes: 1 << 20}, interval: time.Millisecond}
	controller.WithPodLogFollower(func(context.Context, string, string, corev1.PodLogOptions) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("")), nil
	})
	binding, found, err := controller.Adopt(t.Context(), AdoptionRequest{Name: wf.Name, RunID: wf.Annotations[runIDAnnotation], Ref: "feature/restart", SHA: strings.Repeat("c", 40), Trigger: periapsis.TriggerCI, RunTrigger: argoworkflow.RunTriggerBranch})
	if err != nil || !found {
		t.Fatal("verified adoption failed", binding, found, err)
	}
	step := StepResults(wf)[0]
	events := []runprogress.Event{{Version: runprogress.Version, Burn: step.Burn, Step: step.Step, Ordinal: step.Ordinal, Status: runprogress.StepRunning, StartedAt: step.StartedAt}}
	return controller, wf, binding, events
}

func TestRestartCutAdoptedWaitDistinguishesMissingExitFromFailure(t *testing.T) {
	cases := []struct {
		name                string
		interrupted, passed bool
		change              func(*wfv1.Workflow, *wfv1.NodeStatus, *corev1.Pod)
	}{
		{"deadline-remains-deadline", false, false, func(w *wfv1.Workflow, _ *wfv1.NodeStatus, p *corev1.Pod) {
			w.Status.Message = model.ExecutionDeadlineMessage
			p.Name = ""
		}},
		{"lost-pod", true, false, func(_ *wfv1.Workflow, _ *wfv1.NodeStatus, p *corev1.Pod) { p.Name = "" }},
		{"lost-exit-owned-pod", true, false, func(*wfv1.Workflow, *wfv1.NodeStatus, *corev1.Pod) {}},
		{"real-exit-pod-lost", false, false, func(_ *wfv1.Workflow, n *wfv1.NodeStatus, p *corev1.Pod) {
			n.Phase = wfv1.NodeFailed
			n.Outputs = &wfv1.Outputs{ExitCode: ptr.To("1")}
			p.Name = ""
		}},
		{"real-exit-143", false, false, func(_ *wfv1.Workflow, n *wfv1.NodeStatus, p *corev1.Pod) {
			n.Outputs = &wfv1.Outputs{ExitCode: ptr.To("143")}
			p.Name = ""
		}},
		{"real-exit-owned-pod-only", false, false, func(_ *wfv1.Workflow, n *wfv1.NodeStatus, p *corev1.Pod) {
			p.Status.ContainerStatuses[0].State.Terminated = &corev1.ContainerStateTerminated{ExitCode: 2, Reason: "Error", ContainerID: p.Status.ContainerStatuses[0].ContainerID, StartedAt: n.StartedAt, FinishedAt: n.FinishedAt}
		}},
		{"gap-only-passed", false, true, func(w *wfv1.Workflow, n *wfv1.NodeStatus, _ *corev1.Pod) {
			w.Status.Phase = wfv1.WorkflowSucceeded
			n.Phase = wfv1.NodeSucceeded
			n.Outputs = &wfv1.Outputs{ExitCode: ptr.To("0")}
		}},
		{"unsupported-pod-shape", false, false, func(_ *wfv1.Workflow, _ *wfv1.NodeStatus, p *corev1.Pod) {
			p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: "other"})
		}},
		{"foreign-pod-unknown-exit", false, false, func(_ *wfv1.Workflow, _ *wfv1.NodeStatus, p *corev1.Pod) { p.OwnerReferences[0].UID = "foreign" }},
		{"ambiguous-attempt", false, false, func(w *wfv1.Workflow, n *wfv1.NodeStatus, p *corev1.Pod) {
			other := *n
			other.ID = "duplicate"
			other.Outputs = &wfv1.Outputs{ExitCode: ptr.To("1")}
			w.Status.Nodes[other.ID] = other
			p.Name = ""
		}},
		{"malformed-exit", false, false, func(_ *wfv1.Workflow, n *wfv1.NodeStatus, _ *corev1.Pod) {
			n.Outputs = &wfv1.Outputs{ExitCode: ptr.To("not-an-exit")}
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			controller, wf, binding, events := restartCutFixture(t, test.change)
			var output bytes.Buffer
			result, err := controller.WaitResumed(t.Context(), wf.Name, wf.Annotations[runIDAnnotation], &output, events, 100, binding)
			if err != nil || result.Interrupted != test.interrupted || result.Succeeded != test.passed {
				t.Fatalf("wrong restart verdict: %+v err=%v", result, err)
			}
			if !strings.Contains(output.String(), "without a durable pod cursor") {
				t.Fatal("fixture did not exercise adoption gap", output.String())
			}
			if test.interrupted && (!result.Infrastructure || result.FailedStep != "" || !strings.Contains(result.Reason, "no recorded process exit")) {
				t.Fatal("cut retained red test attribution", result)
			}
			if !test.interrupted && !test.passed && (result.Infrastructure || result.FailedStep == "") {
				t.Fatal("real/ambiguous failure became retryable", result)
			}
			fresh, err := controller.TerminalStateResumed(t.Context(), wf.Name, wf.Annotations[runIDAnnotation], binding, nil)
			if err != nil || fresh.Interrupted != test.interrupted || fresh.Succeeded != test.passed {
				t.Fatal("terminal fallback lost classification", fresh, err)
			}
		})
	}
}

func TestRestartCutRequiresVerifiedAdoptionAndBoundTerminalRead(t *testing.T) {
	controller, wf, binding, _ := restartCutFixture(t, func(_ *wfv1.Workflow, _ *wfv1.NodeStatus, p *corev1.Pod) { p.Name = "" })
	ordinary, err := controller.TerminalState(t.Context(), wf.Name)
	if err != nil || ordinary.Interrupted || ordinary.Infrastructure {
		t.Fatal("unknown non-adopted failure was reclassified", ordinary, err)
	}
	ordinaryWait, err := controller.Wait(t.Context(), wf.Name, wf.Annotations[runIDAnnotation], io.Discard)
	if err != nil || ordinaryWait.Interrupted || ordinaryWait.Infrastructure {
		t.Fatal("ordinary wait acquired restart authority", ordinaryWait, err)
	}
	for _, change := range []func(*Adoption){func(a *Adoption) { a.UID = "foreign" }, func(a *Adoption) { a.SpecSHA256 = "foreign" }, func(a *Adoption) { a.Identity = "foreign" }, func(a *Adoption) { a.UID = "" }} {
		bad := binding
		change(&bad)
		if result, err := controller.TerminalStateResumed(t.Context(), wf.Name, wf.Annotations[runIDAnnotation], bad, nil); err == nil || result != nil {
			t.Fatal("foreign binding authorized interruption", result, err)
		}
	}
	if _, err := controller.TerminalStateResumed(t.Context(), wf.Name, "foreign-run", binding, nil); err == nil {
		t.Fatal("foreign run ID accepted")
	}
}

func TestRestartCutRealParallelFailureAndCleanupStayRed(t *testing.T) {
	controller, wf, binding, _ := restartCutFixture(t, func(w *wfv1.Workflow, _ *wfv1.NodeStatus, p *corev1.Pod) {
		p.Name = ""
		for _, original := range w.Status.Nodes {
			other := original
			other.ID, other.Name, other.DisplayName = "real", w.Name+".real", "real"
			other.Phase = wfv1.NodeFailed
			other.Outputs = &wfv1.Outputs{ExitCode: ptr.To("1")}
			other.StartedAt.Time = original.StartedAt.Add(-time.Second)
			w.Status.Nodes[other.ID] = other
			break
		}
	})
	result, err := controller.TerminalStateResumed(t.Context(), wf.Name, wf.Annotations[runIDAnnotation], binding, nil)
	if err != nil || result.Interrupted || result.Infrastructure || result.FailedStep != "real" {
		t.Fatal("cut hid real parallel failure", result, err)
	}
	controller, wf, binding, _ = restartCutFixture(t, func(_ *wfv1.Workflow, _ *wfv1.NodeStatus, p *corev1.Pod) { p.Name = "" })
	result, err = controller.TerminalStateResumed(t.Context(), wf.Name, wf.Annotations[runIDAnnotation], binding, nil)
	if err != nil || !result.Interrupted {
		t.Fatal(result, err)
	}
	result.Steps = append(result.Steps, StepResult{Burn: "cleanup", Step: "delete", Status: runprogress.StepFailed, ExitCode: 1})
	result.classifyRestartCut()
	if result.Interrupted || result.Infrastructure || result.FailedStep != "delete" {
		t.Fatal("hard cleanup became retryable", result)
	}
}

func TestRestartCutDeletionKillNeedsPositiveTimingAndSignal(t *testing.T) {
	for _, mode := range []string{"deleted-during-termination", "deleted-after-failure", "no-delete", "oom", "conflicting-node-exit"} {
		t.Run(mode, func(t *testing.T) {
			controller, wf, binding, _ := restartCutFixture(t, func(_ *wfv1.Workflow, n *wfv1.NodeStatus, p *corev1.Pod) {
				n.Outputs = &wfv1.Outputs{ExitCode: ptr.To("143")}
				term := &corev1.ContainerStateTerminated{ExitCode: 143, Signal: 15, Reason: "Error", ContainerID: p.Status.ContainerStatuses[0].ContainerID, StartedAt: n.StartedAt, FinishedAt: n.FinishedAt}
				p.Status.ContainerStatuses[0].State.Terminated = term
				p.Status.Phase = corev1.PodFailed
				deletion := metav1.NewTime(n.FinishedAt.Add(-time.Second))
				p.DeletionTimestamp = &deletion
				switch mode {
				case "deleted-after-failure":
					deletion.Time = n.FinishedAt.Add(time.Second)
				case "conflicting-node-exit":
					n.Outputs.ExitCode = ptr.To("2")
				case "no-delete":
					p.DeletionTimestamp = nil
				case "oom":
					term.Reason = "OOMKilled"
					term.Signal = 9
					term.ExitCode = 137
					n.Outputs.ExitCode = ptr.To("137")
				}
			})
			result, err := controller.TerminalStateResumed(t.Context(), wf.Name, wf.Annotations[runIDAnnotation], binding, nil)
			if err != nil || result.Interrupted != (mode == "deleted-during-termination") {
				t.Fatal("signal alone or post-failure GC changed verdict", mode, result, err)
			}
		})
	}
}

// Status may settle between two GETs. The fallback must bind the settled
// result too, even when its run annotation and spec still match.
type restartSettleClient struct {
	staticWorkflowClient
	reads       int
	replacement *wfv1.Workflow
}

func (c *restartSettleClient) Get(_ context.Context, _ string, _ metav1.GetOptions) (*wfv1.Workflow, error) {
	c.reads++
	if c.reads == 1 {
		return c.workflow.DeepCopy(), nil
	}
	return c.replacement.DeepCopy(), nil
}
func TestRestartCutFallbackRechecksSettledBindingAndVerdict(t *testing.T) {
	for _, mode := range []string{"changed-uid", "late-real-exit"} {
		t.Run(mode, func(t *testing.T) {
			controller, wf, binding, _ := restartCutFixture(t, func(_ *wfv1.Workflow, _ *wfv1.NodeStatus, p *corev1.Pod) { p.Name = "" })
			next := wf.DeepCopy()
			if mode == "changed-uid" {
				next.UID = "foreign"
			} else {
				for id, node := range next.Status.Nodes {
					node.Outputs = &wfv1.Outputs{ExitCode: ptr.To("2")}
					next.Status.Nodes[id] = node
				}
			}
			controller.workflows = &restartSettleClient{staticWorkflowClient: staticWorkflowClient{workflow: wf}, replacement: next}
			result, err := controller.TerminalStateResumed(t.Context(), wf.Name, wf.Annotations[runIDAnnotation], binding, nil)
			if mode == "changed-uid" {
				if err == nil || result != nil {
					t.Fatal("settled foreign object accepted", result, err)
				}
				return
			}
			if err != nil || result.Interrupted || result.Infrastructure || result.FailedStep == "" {
				t.Fatal("late real failure hidden by old cut evidence", result, err)
			}
		})
	}
}

func TestRestartCutRetainedJournalFailureBindsExactAttempt(t *testing.T) {
	for _, mode := range []string{"exact-failure", "old-attempt", "other-step", "invalid-record"} {
		t.Run(mode, func(t *testing.T) {
			controller, wf, binding, events := restartCutFixture(t, func(_ *wfv1.Workflow, _ *wfv1.NodeStatus, p *corev1.Pod) { p.Name = "" })
			step := StepResults(wf)[0]
			event := runprogress.Event{Version: runprogress.Version, Burn: step.Burn, Step: step.Step, Ordinal: step.Ordinal, Status: runprogress.StepFailed, ExitCode: 2, StartedAt: step.StartedAt, FinishedAt: step.FinishedAt}
			switch mode {
			case "old-attempt":
				start := step.StartedAt.Add(-time.Minute)
				event.StartedAt = &start
			case "other-step":
				event.Step = "other"
			case "invalid-record":
				event.Version = -1
			}
			retained := append(events, event)
			// Invalid journal input remains refused by Wait's existing validation;
			// the terminal reader must also never use an invalid record as evidence.
			if mode != "invalid-record" {
				result, err := controller.WaitResumed(t.Context(), wf.Name, wf.Annotations[runIDAnnotation], io.Discard, retained, 100, binding)
				if err != nil || result.Interrupted != (mode != "exact-failure") {
					t.Fatal("retained failure lost or wrong attempt bound", result, err)
				}
			}
			result, err := controller.TerminalStateResumed(t.Context(), wf.Name, wf.Annotations[runIDAnnotation], binding, retained)
			if err != nil || result.Interrupted != (mode != "exact-failure") {
				t.Fatal("fallback journal evidence lost or misbound", result, err)
			}
			if mode == "exact-failure" && (!result.Steps[0].ExitCodeKnown || result.Steps[0].ExitCode != 2) {
				t.Fatal("real recorded exit erased", result)
			}
		})
	}
}

func TestRestartCutSettlingReadCannotEraseKnownFailure(t *testing.T) {
	for _, path := range []string{"wait", "terminal"} {
		t.Run(path, func(t *testing.T) {
			controller, wf, binding, _ := restartCutFixture(t, func(_ *wfv1.Workflow, n *wfv1.NodeStatus, p *corev1.Pod) {
				n.Outputs = &wfv1.Outputs{ExitCode: ptr.To("2")}
				p.Name = ""
			})
			incomplete := wf.DeepCopy()
			for id, node := range incomplete.Status.Nodes {
				node.Outputs = nil
				incomplete.Status.Nodes[id] = node
			}
			controller.workflows = &restartSettleClient{staticWorkflowClient: staticWorkflowClient{workflow: wf}, replacement: incomplete}
			var result *Completion
			var err error
			if path == "wait" {
				value, e := controller.WaitResumed(t.Context(), wf.Name, wf.Annotations[runIDAnnotation], io.Discard, nil, 0, binding)
				result, err = &value, e
			} else {
				result, err = controller.TerminalStateResumed(t.Context(), wf.Name, wf.Annotations[runIDAnnotation], binding, nil)
			}
			if err != nil || result.Interrupted || result.Infrastructure || !result.Steps[0].ExitCodeKnown || result.Steps[0].ExitCode != 2 {
				t.Fatal("settle erased an observed failure", result, err)
			}
		})
	}
}

func TestRestartCutHardCleanupVerdictWins(t *testing.T) {
	for _, path := range []string{"wait", "terminal"} {
		t.Run(path, func(t *testing.T) {
			controller, wf, binding, _ := restartCutFixture(t, func(w *wfv1.Workflow, _ *wfv1.NodeStatus, p *corev1.Pod) {
				p.Name = ""
				w.Annotations[argoworkflow.CleanupVerdictAnnotation] = "invalid"
			})
			var result *Completion
			var err error
			if path == "wait" {
				value, e := controller.WaitResumed(t.Context(), wf.Name, wf.Annotations[runIDAnnotation], io.Discard, nil, 0, binding)
				result, err = &value, e
			} else {
				result, err = controller.TerminalStateResumed(t.Context(), wf.Name, wf.Annotations[runIDAnnotation], binding, nil)
			}
			if err != nil || result.Interrupted || result.Infrastructure || result.FailedStep != "cleanup-verdict" {
				t.Fatal("restart hid hard cleanup failure", result, err)
			}
		})
	}
}

func TestRestartCutConflictingExitEvidenceStaysRed(t *testing.T) {
	for _, source := range []string{"journal", "earlier-snapshot"} {
		t.Run(source, func(t *testing.T) {
			controller, wf, binding, _ := restartCutFixture(t, func(_ *wfv1.Workflow, n *wfv1.NodeStatus, p *corev1.Pod) {
				n.Outputs = &wfv1.Outputs{ExitCode: ptr.To("143")}
				p.Status.Phase = corev1.PodFailed
				deletion := metav1.NewTime(n.FinishedAt.Add(-time.Second))
				p.DeletionTimestamp = &deletion
				p.Status.ContainerStatuses[0].State.Terminated = &corev1.ContainerStateTerminated{ExitCode: 143, Signal: 15, Reason: "Error", ContainerID: p.Status.ContainerStatuses[0].ContainerID, StartedAt: n.StartedAt, FinishedAt: n.FinishedAt}
			})
			step := StepResults(wf)[0]
			var events []runprogress.Event
			if source == "journal" {
				events = []runprogress.Event{{Version: runprogress.Version, Burn: step.Burn, Step: step.Step, Ordinal: step.Ordinal, Status: runprogress.StepFailed, ExitCode: 2, StartedAt: step.StartedAt, FinishedAt: step.FinishedAt}}
			} else {
				earlier := wf.DeepCopy()
				for id, node := range earlier.Status.Nodes {
					node.Outputs = &wfv1.Outputs{ExitCode: ptr.To("2")}
					earlier.Status.Nodes[id] = node
				}
				controller.workflows = &restartSettleClient{staticWorkflowClient: staticWorkflowClient{workflow: earlier}, replacement: wf}
			}
			result, err := controller.TerminalStateResumed(t.Context(), wf.Name, wf.Annotations[runIDAnnotation], binding, events)
			if err != nil || result.Interrupted || result.Infrastructure {
				t.Fatal("deletion hid conflicting real failure evidence", result, err)
			}
		})
	}
}
