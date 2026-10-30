package argojob

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	"github.com/oberthci/oberth/internal/runprogress"
)

func TestRestartCutR2RequiresPositivePodObservation(t *testing.T) {
	for _, mode := range []string{"list-error", "later-page-error", "changed-resource-version", "never-executed", "absent", "readable-missing-termination", "readable-zero-exit"} {
		t.Run(mode, func(t *testing.T) {
			c, wf, binding, events := restartCutFixture(t, func(_ *wfv1.Workflow, _ *wfv1.NodeStatus, p *corev1.Pod) {
				if mode != "readable-missing-termination" && mode != "readable-zero-exit" {
					p.Name = ""
				}
				if mode == "readable-zero-exit" {
					p.Status.ContainerStatuses[0].State.Terminated = &corev1.ContainerStateTerminated{ExitCode: 0, Reason: "Completed", ContainerID: p.Status.ContainerStatuses[0].ContainerID, StartedAt: metav1.NewTime(time.Date(2026, 10, 15, 11, 59, 0, 0, time.UTC)), FinishedAt: metav1.NewTime(time.Date(2026, 10, 15, 11, 59, 59, 0, time.UTC))}
				}
			})
			kube := c.kube.(*fake.Clientset)
			calls := 0
			switch mode {
			case "list-error", "later-page-error", "changed-resource-version":
				kube.PrependReactor("list", "pods", func(action ktesting.Action) (bool, runtime.Object, error) {
					calls++
					if mode == "list-error" {
						return true, nil, errors.New("API unavailable")
					}
					if calls%2 == 1 {
						return true, &corev1.PodList{ListMeta: metav1.ListMeta{ResourceVersion: "1", Continue: "next"}}, nil
					}
					if mode == "later-page-error" {
						return true, nil, errors.New("later page unavailable")
					}
					return true, &corev1.PodList{ListMeta: metav1.ListMeta{ResourceVersion: "2"}}, nil
				})
			case "never-executed":
				// Even a Pod-type node is not executed evidence without a Pod name.
				completion := completionFor(wf)
				completion.Steps[0].PodName = ""
				c.collectRestartStepDiagnostics(t.Context(), wf, &completion, binding.RestartedAt)
				completion.classifyRestartCut()
				if completion.Interrupted || completion.Infrastructure || completion.Steps[0].Termination.Unavailable != "not-executed" {
					t.Fatal("uncreated Pod became cut", completion)
				}
				return
			}
			want := mode == "absent" || mode == "readable-missing-termination"
			for _, path := range []string{"wait", "terminal"} {
				calls = 0
				var got *Completion
				var err error
				if path == "wait" {
					value, e := c.WaitResumed(t.Context(), wf.Name, wf.Annotations[runIDAnnotation], io.Discard, events, 0, binding)
					got, err = &value, e
				} else {
					got, err = c.TerminalStateResumed(t.Context(), wf.Name, wf.Annotations[runIDAnnotation], binding, events)
				}
				if err != nil || got.Interrupted != want || (!want && (got.Infrastructure || got.FailedStep == "")) {
					t.Fatal("ambiguous observation changed verdict", path, got, err)
				}
				d := got.Steps[0].Termination
				if d.Validate() != nil || got.Steps[0].Timing.Validate() != nil {
					t.Fatal("invalid diagnostic/timing projection", d, got.Steps[0].Timing)
				}
				if mode == "list-error" && d.Unavailable != "observation-unavailable" {
					t.Fatal(d)
				}
				if mode == "absent" && d.Unavailable != "pod-absent" {
					t.Fatal(d)
				}
			}
		})
	}
}

func TestRestartCutR2RequiresStepBeforeThisRestart(t *testing.T) {
	for _, mode := range []string{"before", "equal", "after", "unknown-boundary", "unknown-start"} {
		t.Run(mode, func(t *testing.T) {
			c, wf, binding, _ := restartCutFixture(t, func(_ *wfv1.Workflow, _ *wfv1.NodeStatus, p *corev1.Pod) { p.Name = "" })
			for _, node := range wf.Status.Nodes {
				binding.RestartedAt = node.StartedAt.Time
				if mode == "before" {
					binding.RestartedAt = binding.RestartedAt.Add(time.Second)
				}
				if mode == "after" {
					binding.RestartedAt = binding.RestartedAt.Add(-time.Second)
				}
				if mode == "unknown-start" {
					node.StartedAt = metav1.Time{}
					wf.Status.Nodes[node.ID] = node
				}
			}
			if mode == "unknown-boundary" {
				binding.RestartedAt = time.Time{}
			}
			result, err := c.TerminalStateResumed(t.Context(), wf.Name, wf.Annotations[runIDAnnotation], binding, nil)
			want := mode == "before" || mode == "equal"
			if err != nil || result.Interrupted != want {
				t.Fatal("restart attribution crossed process boundary", result, err)
			}
		})
	}
}

func TestRestartCutR2NodeOOMOrDeadlineCannotBecomeCut(t *testing.T) {
	for _, message := range []string{"main: OOMKilled", "pod was active on the node longer than the specified deadline", "DeadlineExceeded"} {
		t.Run(message, func(t *testing.T) {
			c, wf, binding, _ := restartCutFixture(t, func(_ *wfv1.Workflow, n *wfv1.NodeStatus, p *corev1.Pod) { p.Name = ""; n.Message = message })
			result, err := c.TerminalStateResumed(t.Context(), wf.Name, wf.Annotations[runIDAnnotation], binding, nil)
			if err != nil || result.Interrupted || result.Infrastructure {
				t.Fatal("hard failure became cut", result, err)
			}
		})
	}
}

func TestRestartCutR2JournalSurvivesSecondRestartWithoutInventingExit(t *testing.T) {
	c, wf, binding, events := restartCutFixture(t, func(_ *wfv1.Workflow, _ *wfv1.NodeStatus, p *corev1.Pod) { p.Name = "" })
	var output bytes.Buffer
	first, err := c.WaitResumed(t.Context(), wf.Name, wf.Annotations[runIDAnnotation], &output, events, 0, binding)
	if err != nil || !first.Interrupted {
		t.Fatal(first, err)
	}
	var retained []runprogress.Event
	for _, line := range strings.Split(output.String(), "\n") {
		event, found, err := runprogress.ParseMarker([]byte(line))
		if err != nil {
			t.Fatal(err)
		}
		if found {
			retained = append(retained, event)
			if event.Status == runprogress.StepFailed && event.ExitCode != 0 {
				t.Fatal("unknown exit journaled as real", event)
			}
		}
	}
	if len(retained) == 0 {
		t.Fatal("no durable progress emitted")
	}
	second, err := c.TerminalStateResumed(t.Context(), wf.Name, wf.Annotations[runIDAnnotation], binding, retained)
	if err != nil || !second.Interrupted {
		t.Fatal("second restart fabricated real failure", second, err)
	}
	// A genuinely observed exit must still be journaled, and must remain red.
	for id, node := range wf.Status.Nodes {
		node.Outputs = &wfv1.Outputs{ExitCode: ptr.To("2")}
		wf.Status.Nodes[id] = node
	}
	output.Reset()
	real, err := c.WaitResumed(t.Context(), wf.Name, wf.Annotations[runIDAnnotation], &output, nil, 0, binding)
	if err != nil || real.Interrupted {
		t.Fatal(real, err)
	}
	retained = nil
	for _, line := range strings.Split(output.String(), "\n") {
		event, found, err := runprogress.ParseMarker([]byte(line))
		if err != nil {
			t.Fatal(err)
		}
		if found {
			retained = append(retained, event)
		}
	}
	if len(retained) == 0 || retained[len(retained)-1].ExitCode != 2 {
		t.Fatal("real exit lost from journal", retained)
	}
	for id, node := range wf.Status.Nodes {
		node.Outputs = nil
		wf.Status.Nodes[id] = node
	}
	red, err := c.TerminalStateResumed(context.Background(), wf.Name, wf.Annotations[runIDAnnotation], binding, retained)
	if err != nil || red.Interrupted || !red.Steps[0].ExitCodeKnown {
		t.Fatal("second restart erased real failure", red, err)
	}
}

func TestRestartCutR2AdoptionPinsProcessInitialization(t *testing.T) {
	wf, request := adoptionWorkflow()
	config := testConfig()
	config.Namespace = wf.Namespace
	c, err := NewController(&staticWorkflowClient{workflow: wf}, nil, config)
	if err != nil {
		t.Fatal(err)
	}
	binding, found, err := c.Adopt(t.Context(), request)
	if err != nil || !found || binding.RestartedAt.IsZero() || !binding.RestartedAt.Equal(serverStartedAt) {
		t.Fatal("adoption lost process boundary", binding, found, err)
	}
}
