package argojob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	"github.com/oberthci/oberth/internal/runprogress"
)

// Fixtures choose opaque Pod names; no copy of Argo's naming convention is used.
func fixturePodName(_ *wfv1.Workflow, node wfv1.NodeStatus) string { return node.ID }

func fixtureStepPods(workflow *wfv1.Workflow) map[string]*corev1.Pod {
	if workflow.UID == "" {
		workflow.UID = "fixture-workflow-uid"
	}
	pods := make(map[string]*corev1.Pod)
	for _, node := range workflow.Status.Nodes {
		if node.Type != wfv1.NodeTypePod {
			continue
		}
		name := fixturePodName(workflow, node)
		pods[name] = &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: workflow.Namespace, UID: types.UID("fixture-" + name),
			Labels:          map[string]string{"workflows.argoproj.io/workflow": workflow.Name},
			Annotations:     map[string]string{"workflows.argoproj.io/node-id": node.ID, "workflows.argoproj.io/node-name": node.Name},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "argoproj.io/v1alpha1", Kind: "Workflow", Name: workflow.Name, UID: workflow.UID, Controller: ptr.To(true)}}}}
	}
	return pods
}

func fixtureKube(workflow *wfv1.Workflow) *fake.Clientset {
	var objects []runtime.Object
	for _, pod := range fixtureStepPods(workflow) {
		objects = append(objects, pod)
	}
	return fake.NewClientset(objects...)
}

func fixtureStepAttempts(workflow *wfv1.Workflow) []StepResult {
	steps := stepAttempts(workflow)
	if workflow != nil {
		bindStepPodNames(workflow, steps, fixtureStepPods(workflow))
	}
	return steps
}

func fixtureStepResults(workflow *wfv1.Workflow) []StepResult {
	return finalStepResults(fixtureStepAttempts(workflow))
}

func newFixtureLogFollowers(ctx context.Context, reader PodLogFollower, output io.Writer, events []runprogress.Event) *stepLogFollowers {
	logs := newStepLogFollowers(ctx, reader, output, events)
	logs.steps = func(_ context.Context, workflow *wfv1.Workflow) []StepResult { return fixtureStepAttempts(workflow) }
	return logs
}

func TestStepPodDiscoveryUsesAnnotationsAndOwnerWithOpaqueNames(t *testing.T) {
	for _, format := range []string{"", "v1", "v2"} {
		t.Run(format, func(t *testing.T) {
			workflow, node, pod, _ := diagnosticFixture()
			workflow.Annotations["workflows.argoproj.io/pod-name-format"] = format
			pod.Name = "opaque-observed-step"
			controller := &Controller{kube: fake.NewClientset(pod)}
			steps := controller.observedStepAttempts(t.Context(), workflow)
			if len(steps) != 1 || steps[0].PodName != pod.Name || steps[0].NodeID != node.ID {
				t.Fatalf("Pod was not discovered from native identity: %+v", steps)
			}
			if StepResults(workflow)[0].PodName != "" {
				t.Fatal("status-only projection guessed a Pod name")
			}
		})
	}
}

func TestStepPodDiscoveryRejectsForeignAmbiguousAndMissingPods(t *testing.T) {
	for _, mode := range []string{"absent", "owner", "namespace", "node-id", "node-name", "duplicate"} {
		t.Run(mode, func(t *testing.T) {
			workflow, _, pod, _ := diagnosticFixture()
			pod.Name = "opaque-observed-step"
			var objects []runtime.Object
			switch mode {
			case "owner":
				pod.OwnerReferences[0].UID = "another-workflow"
			case "namespace":
				pod.Namespace = "another-namespace"
			case "node-id":
				pod.Annotations["workflows.argoproj.io/node-id"] = "another-node"
			case "node-name":
				pod.Annotations["workflows.argoproj.io/node-name"] = "another-attempt"
			case "duplicate":
				other := pod.DeepCopy()
				other.Name, other.UID = "second-observed-step", "second-pod-uid"
				objects = append(objects, other)
			}
			if mode != "absent" {
				objects = append(objects, pod)
			}
			controller := &Controller{kube: fake.NewClientset(objects...)}
			steps := controller.observedStepAttempts(t.Context(), workflow)
			if len(steps) != 1 || steps[0].PodName != "" {
				t.Fatalf("unsafe or guessed binding: %+v", steps)
			}
		})
	}
}

func TestStepLogsAndDiagnosticsUseObservedOpaquePodName(t *testing.T) {
	workflow, _, pod, _ := diagnosticFixture()
	pod.Name = "opaque-observed-step"
	var output strings.Builder
	controller := &Controller{kube: fake.NewClientset(pod), logs: func(_ context.Context, _, name, _ string) (io.ReadCloser, error) {
		if name != pod.Name {
			t.Fatalf("log read guessed a name: %q", name)
		}
		return io.NopCloser(strings.NewReader("native step output\n")), nil
	}}
	if err := controller.publishLogs(t.Context(), workflow, newProgressReporter(&output), &output, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "native step output") {
		t.Fatal(output.String())
	}
	completion := completionFor(workflow)
	controller.collectRestartStepDiagnostics(t.Context(), workflow, &completion, time.Time{})
	if len(completion.Steps) != 1 || completion.Steps[0].PodName != pod.Name || !completion.Steps[0].Termination.Available {
		t.Fatalf("diagnostic did not bind observed Pod: %+v", completion.Steps)
	}
}

func TestTerminalStepLogDiscoveryRetriesIncompleteLists(t *testing.T) {
	for _, mode := range []string{"error", "timeout", "incomplete", "changed-version", "missing-pod"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				workflow, _, pod, _ := diagnosticFixture()
				workflow.Status.Phase = wfv1.WorkflowSucceeded
				pod.Name = "opaque-terminal-pod"
				kube := fake.NewClientset(pod)
				calls := 0
				kube.PrependReactor("list", "pods", func(action ktesting.Action) (bool, runtime.Object, error) {
					calls++
					if calls == 1 {
						switch mode {
						case "error":
							return true, nil, errors.New("temporarily unavailable")
						case "timeout":
							return true, nil, context.DeadlineExceeded
						case "incomplete", "changed-version":
							return true, &corev1.PodList{ListMeta: metav1.ListMeta{ResourceVersion: "1", Continue: "page2"}}, nil
						case "missing-pod":
							return true, &corev1.PodList{}, nil
						}
					}
					if calls == 2 && (mode == "incomplete" || mode == "changed-version") {
						if mode == "incomplete" {
							return true, nil, errors.New("page unavailable")
						}
						return true, &corev1.PodList{ListMeta: metav1.ListMeta{ResourceVersion: "2"}, Items: []corev1.Pod{*pod}}, nil
					}
					return false, nil, nil
				})
				reads := 0
				var output strings.Builder
				controller := &Controller{kube: kube, logs: func(_ context.Context, _, name, _ string) (io.ReadCloser, error) {
					reads++
					if name != pod.Name {
						t.Fatalf("guessed Pod name %q", name)
					}
					return io.NopCloser(strings.NewReader("recovered terminal output\n")), nil
				}}
				reporter := newProgressReporter(&output)
				for range 2 {
					if err := controller.publishLogs(t.Context(), workflow, reporter, &output, nil); err != nil {
						t.Fatal(err)
					}
				}
				if calls < 2 || reads != 1 || strings.Count(output.String(), "recovered terminal output") != 1 {
					t.Fatalf("terminal log was lost or duplicated: lists=%d reads=%d output=%s", calls, reads, output.String())
				}
			})
		})
	}
}

func TestStepLogReadStaysEligibleAfterTerminalEvent(t *testing.T) {
	workflow, node, pod, _ := diagnosticFixture()
	workflow.Status.Phase = wfv1.WorkflowRunning
	kube := fake.NewClientset(pod)
	calls, reads := 0, 0
	kube.PrependReactor("list", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
		calls++
		if calls == 1 {
			return true, nil, context.DeadlineExceeded
		}
		return false, nil, nil
	})
	var output strings.Builder
	controller := &Controller{kube: kube, logs: func(context.Context, string, string, string) (io.ReadCloser, error) {
		reads++
		return io.NopCloser(strings.NewReader("delayed output\n")), nil
	}}
	reporter := newProgressReporter(&output)
	if err := controller.publishLogs(t.Context(), workflow, reporter, &output, nil); err != nil {
		t.Fatal(err)
	}
	if reads != 0 || reporter.replayed[node.ID] {
		t.Fatal("unread log was marked replayed")
	}
	workflow.Status.Phase = wfv1.WorkflowSucceeded
	if err := controller.publishLogs(t.Context(), workflow, reporter, &output, nil); err != nil {
		t.Fatal(err)
	}
	if reads != 1 || !reporter.replayed[node.ID] || !strings.Contains(output.String(), "delayed output") {
		t.Fatalf("terminal event suppressed later log read: %s", output.String())
	}
}

func TestStepLogDiscoveryDoesNotUseTimingSnapshotCap(t *testing.T) {
	workflow, _, pod, _ := diagnosticFixture()
	workflow.Status.Phase = wfv1.WorkflowSucceeded
	kube := fake.NewClientset()
	kube.PrependReactor("list", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
		list := &corev1.PodList{Items: []corev1.Pod{*pod}}
		for i := range 2100 {
			list.Items = append(list.Items, corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("other-%d", i)}})
		}
		return true, list, nil
	})
	controller := &Controller{kube: kube}
	steps := controller.observedStepAttempts(t.Context(), workflow)
	if len(steps) != 1 || steps[0].PodName != pod.Name {
		t.Fatalf("large Pod list lost the binding: %+v", steps)
	}
}

func TestUnboundTerminalStepLogsAreExplicitlyUnavailable(t *testing.T) {
	for _, live := range []bool{false, true} {
		t.Run(fmt.Sprint(live), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				workflow, node, _, _ := diagnosticFixture()
				workflow.Status.Phase = wfv1.WorkflowSucceeded
				kube := fake.NewClientset()
				kube.PrependReactor("list", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
					return true, nil, context.DeadlineExceeded
				})
				var output strings.Builder
				controller := &Controller{kube: kube}
				reporter := newProgressReporter(&output)
				var followers *stepLogFollowers
				if live {
					followers = newStepLogFollowers(t.Context(), nil, &output, nil)
					followers.steps = controller.observedStepAttempts
					defer followers.close()
				}
				if err := controller.publishLogs(t.Context(), workflow, reporter, &output, followers); err != nil {
					t.Fatal(err)
				}
				if followers != nil {
					followers.drain(workflow)
				}
				if !strings.Contains(output.String(), "step log unavailable") || reporter.replayed[node.ID] {
					t.Fatalf("unread log silently completed: %s", output.String())
				}
				if live && (!strings.Contains(output.String(), `"state":"unavailable"`) || strings.Contains(output.String(), `"state":"complete"`)) {
					t.Fatalf("missing unavailable delivery record: %s", output.String())
				}
			})
		})
	}
}

func TestLiveTerminalDrainRecoversDiscoveryWithoutDuplicatePublishList(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		workflow, _, pod, _ := diagnosticFixture()
		workflow.Status.Phase = wfv1.WorkflowRunning
		pod.Name = "opaque-live-terminal-pod"
		kube := fake.NewClientset(pod)
		lists, reads := 0, 0
		kube.PrependReactor("list", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
			lists++
			if lists == 1 {
				return true, nil, context.DeadlineExceeded
			}
			return false, nil, nil
		})
		var output strings.Builder
		controller := &Controller{kube: kube}
		followers := newStepLogFollowers(t.Context(), func(_ context.Context, _, name string, options corev1.PodLogOptions) (io.ReadCloser, error) {
			reads++
			if name != pod.Name || options.Follow {
				t.Errorf("wrong final read: %q %+v", name, options)
			}
			return io.NopCloser(strings.NewReader(stamped("recovered live output"))), nil
		}, &output, nil)
		followers.steps = controller.observedStepAttempts
		defer followers.close()
		if err := controller.publishLogs(t.Context(), workflow, newProgressReporter(&output), &output, followers); err != nil {
			t.Fatal(err)
		}
		if lists != 1 {
			t.Fatalf("publish rediscovered Pods: %d lists", lists)
		}
		workflow.Status.Phase = wfv1.WorkflowSucceeded
		followers.drain(workflow)
		if reads != 1 || strings.Count(output.String(), "recovered live output") != 1 || !strings.Contains(output.String(), `"state":"complete"`) {
			t.Fatalf("drain lost or duplicated logs: reads=%d output=%s", reads, output.String())
		}
	})
}
