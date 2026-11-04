package argojob

import (
	"context"
	"errors"
	"fmt"
	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/measured"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/resourcesampler"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/storetest"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type reviewSnapshot struct{ value resourcesampler.Snapshot }

func (*reviewSnapshot) Interval() time.Duration { return time.Second }
func (s *reviewSnapshot) Snapshot(context.Context, string) (resourcesampler.Snapshot, error) {
	return s.value, nil
}

func TestReviewMissingSupersededAttemptCannotBorrowRetryCoverage(t *testing.T) {
	for _, mode := range []string{"covered", "missing-retry", "foreign-retry", "unavailable-pods", "skipped-unexecuted", "incomplete-tree", "dangling-tree"} {
		t.Run(mode, func(t *testing.T) {
			missingFailedRetry := mode == "missing-retry" || mode == "foreign-retry" || mode == "unavailable-pods"
			db, e := storetest.Open(t.Context(), filepath.Join(t.TempDir(), "test.sqlite"), store.Options{})
			if e != nil {
				t.Fatal(e)
			}
			defer db.Close()
			up, e := storetest.CreateUpstream(db, t.Context(), model.UpstreamSpec{Name: "github", Kind: "ssh", BaseURL: "ssh://git@github.com/example"})
			if e != nil {
				t.Fatal(e)
			}
			repo, e := db.CreateRepository(t.Context(), model.RepositorySpec{Name: "coverage", UpstreamID: up.ID, DefaultBranch: "main"})
			if e != nil {
				t.Fatal(e)
			}
			for i := range 5 {
				run, e := db.EnqueueRun(t.Context(), model.RunSpec{RepoID: repo.ID, RefKind: model.RefBranch, Ref: "main", SHA: fmt.Sprintf("%040x", i+1), Actor: "test", Trigger: "push"})
				if e != nil {
					t.Fatal(e)
				}
				wf, node, pod, _ := diagnosticFixtureAt(time.Now().UTC().Truncate(time.Second))
				wf.Annotations[runIDAnnotation] = run.ID
				wf.UID = types.UID(fmt.Sprintf("11111111-1111-4111-8111-%012d", i+1))
				pod.UID = types.UID(fmt.Sprintf("22222222-2222-4222-8222-%012d", i+1))
				pod.OwnerReferences[0].UID = wf.UID
				node.ID = "workflow-test-1234"
				node.Name = wf.Name + ".build.compile"
				node.DisplayName = "compile"
				node.TemplateName = "compile"
				node.BoundaryID = "build"
				node.Outputs = &wfv1.Outputs{ExitCode: ptr.To("0")}
				start := time.Now().UTC().Truncate(time.Second).Add(-10 * time.Second)
				end := start.Add(10 * time.Second)
				node.StartedAt = metav1.NewTime(start)
				node.FinishedAt = metav1.NewTime(end)
				wf.Status.Nodes = wfv1.Nodes{
					"root":  {ID: "root", Name: wf.Name, Type: wfv1.NodeTypeDAG, Phase: wfv1.NodeSucceeded},
					"build": {ID: "build", Name: wf.Name + ".build", DisplayName: "build", Type: wfv1.NodeTypeDAG, BoundaryID: "root", Phase: wfv1.NodeSucceeded},
				}
				if missingFailedRetry && i == 4 {
					node.Name += "(1)"
					node.DisplayName += "(1)"
					previous := node
					previous.ID = "workflow-test-0123"
					previous.Name = wf.Name + ".build.compile(0)"
					previous.DisplayName = "compile(0)"
					previous.Phase = wfv1.NodeFailed
					previous.StartedAt = metav1.NewTime(start.Add(-20 * time.Second))
					previous.FinishedAt = metav1.NewTime(start.Add(-time.Second))
					previous.Outputs = &wfv1.Outputs{ExitCode: ptr.To("137")}
					wf.Status.Nodes[previous.ID] = previous
					wf.Status.Nodes["retry"] = wfv1.NodeStatus{ID: "retry", Name: wf.Name + ".build.compile", DisplayName: "compile", Type: wfv1.NodeTypeRetry, BoundaryID: "build", Phase: wfv1.NodeSucceeded, Children: []string{previous.ID, node.ID}}
				}
				if mode == "skipped-unexecuted" && i == 4 {
					wf.Status.Nodes["unexecuted"] = wfv1.NodeStatus{ID: "unexecuted", Name: wf.Name + ".build.unexecuted", DisplayName: "unexecuted", Type: wfv1.NodeTypePod, TemplateName: "unexecuted", BoundaryID: "build", Phase: wfv1.NodeSkipped}
				}
				if mode == "dangling-tree" && i == 4 {
					b := wf.Status.Nodes["build"]
					b.Children = []string{node.ID, "missing-child"}
					wf.Status.Nodes["build"] = b
				}
				if mode == "incomplete-tree" && i == 4 {
					wf.Status.OffloadNodeStatusVersion = "1"
				}
				pod.Name = PodName(wf, node)
				pod.Annotations["workflows.argoproj.io/node-id"] = node.ID
				pod.Annotations["workflows.argoproj.io/node-name"] = node.Name
				pod.Spec.NodeName = "node-a"
				pod.Spec.Containers = pod.Spec.Containers[:1]
				pod.Spec.Containers[0].Resources = corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("256Mi"), corev1.ResourceEphemeralStorage: resource.MustParse("128Mi")}, Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("1Gi"), corev1.ResourceEphemeralStorage: resource.MustParse("512Mi")}}
				pod.Status.ContainerStatuses = pod.Status.ContainerStatuses[:1]
				pod.Status.StartTime = ptr.To(metav1.NewTime(start))
				pod.CreationTimestamp = metav1.NewTime(start)
				if _, e = db.SetRunWorkflowName(t.Context(), run.ID, wf.Name); e != nil {
					t.Fatal(e)
				}
				source := &reviewSnapshot{}
				c := &Controller{config: Config{Namespace: wf.Namespace}, resourceSource: source, resourceRecorder: db}
				finished := map[string]time.Time{}
				for _, seconds := range []int{1, 3, 6, 10} {
					sampleAt := start.Add(time.Duration(seconds) * time.Second)
					node.Phase = wfv1.NodeRunning
					pod.Status.Phase = corev1.PodRunning
					pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(start)}}
					if seconds == 10 {
						node.Phase = wfv1.NodeSucceeded
						pod.Status.Phase = corev1.PodSucceeded
						pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Completed", ExitCode: 0, StartedAt: metav1.NewTime(start), FinishedAt: metav1.NewTime(end), ContainerID: pod.Status.ContainerStatuses[0].ContainerID}}
					}
					wf.Status.Nodes[node.ID] = node
					if seconds == 10 {
						wf.Status.Phase = wfv1.WorkflowSucceeded
					} else {
						wf.Status.Phase = wfv1.WorkflowRunning
					}
					cgroup := "/kubepods/pod" + string(pod.UID) + "/" + strings.Repeat("b", 64)
					raw := fmt.Sprintf("container_memory_usage_bytes{namespace=%q,pod=%q,container=\"main\",id=%q} 268435456 %d\ncontainer_cpu_usage_seconds_total{namespace=%q,pod=%q,container=\"main\",id=%q,cpu=\"total\"} %g %d\n", pod.Namespace, pod.Name, cgroup, sampleAt.UnixMilli(), pod.Namespace, pod.Name, cgroup, float64(seconds)/2, sampleAt.UnixMilli())
					metrics, e := resourcesampler.ParseMetrics([]byte(raw))
					if e != nil {
						t.Fatal(e)
					}
					source.value = resourcesampler.Snapshot{MetricsOK: true, Metrics: metrics, SummaryOK: true}
					source.value.Summary.Pods = []resourcesampler.PodStats{{PodRef: struct{ Name, Namespace, UID string }{Name: pod.Name, Namespace: pod.Namespace, UID: string(pod.UID)}, EphemeralStorage: &struct {
						Time      time.Time `json:"time"`
						UsedBytes *uint64   `json:"usedBytes"`
					}{Time: sampleAt, UsedBytes: ptr.To(uint64(100 << 20))}}}
					kube := fake.NewClientset(pod)
					if mode == "foreign-retry" && i == 4 {
						foreign := pod.DeepCopy()
						previous := wf.Status.Nodes["workflow-test-0123"]
						foreign.Name = PodName(wf, previous)
						foreign.UID = "44444444-4444-4444-8444-444444444444"
						foreign.Annotations["workflows.argoproj.io/node-id"] = previous.ID
						foreign.Annotations["workflows.argoproj.io/node-name"] = previous.Name
						foreign.OwnerReferences[0].UID = "55555555-5555-4555-8555-555555555555"
						kube = fake.NewClientset(pod, foreign)
					}
					unavailable := mode == "unavailable-pods" && i == 4
					if unavailable {
						kube.PrependReactor("list", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
							return true, nil, errors.New("pod inventory unavailable")
						})
					}
					c.kube = kube
					e = c.sampleResourceUsageWithState(t.Context(), wf, run.ID, finished)
					if (e != nil) != unavailable {
						t.Fatalf("unexpected collector result: %v", e)
					}

				}
				results := StepResults(wf)
				foundFinal := false
				for _, result := range results {
					if result.NodeID == node.ID {
						foundFinal = true
					}
				}
				if !foundFinal || (mode != "skipped-unexecuted" && len(results) != 1) {
					t.Fatalf("final step selection: %+v", results)
				}
				if missingFailedRetry && i == 4 && len(stepAttempts(wf)) != 2 {
					t.Fatal("missing failed-attempt fixture")
				}
				if _, e = db.PutStepResult(t.Context(), model.StepResult{RunID: run.ID, Stage: "build", Step: "compile", Status: model.StepPassed, ExitCode: 0, DeclaredSize: "M"}); e != nil {
					t.Fatal(e)
				}
				if _, e = db.FinishRun(t.Context(), run.ID, model.RunResult{Status: model.RunPassed}); e != nil {
					t.Fatal(e)
				}
			}
			h, e := db.RunResources(t.Context(), repo.ID, store.RunResourceFilter{Ref: "main", Runs: 30})
			if e != nil {
				t.Fatal(e)
			}
			for j := range h.Observations {
				h.Observations[j].Owner = repo.Name
			}
			for j := range h.SafetyObservations {
				h.SafetyObservations[j].Owner = repo.Name
			}
			for j := range h.Dispositions {
				h.Dispositions[j].Owner = repo.Name
			}
			steps := measured.BuildWithSafety(h.Observations, nil, h.SafetyObservations, h.Dispositions, h.AttemptGaps)
			if len(steps) != 1 {
				t.Fatalf("steps: %+v", steps)
			}
			if missingFailedRetry || mode == "incomplete-tree" || mode == "dangling-tree" {
				if !h.SafetyIncomplete && steps[0].Suggested != nil {
					t.Fatalf("known executed failed retry vanished: usage=%d dispositions=%d safety_incomplete=%t advice=%+v", len(h.Observations), len(h.Dispositions), h.SafetyIncomplete, steps[0].Suggested)
				}
				if missingFailedRetry {
					if steps[0].Suggested != nil || steps[0].Flags.OOM || steps[0].SizingAcceptance.CensoredAttempts != 0 {
						t.Fatalf("unknown failed retry lost its specific refusal or acquired a physical cause: %+v", steps[0])
					}
					if (mode == "missing-retry" || mode == "foreign-retry") && h.SafetyIncomplete {
						t.Fatal("global inventory refusal masked exact-node reconciliation")
					}
					found := false
					for _, gap := range h.AttemptGaps {
						if gap.NodeID == "workflow-test-0123" && gap.WorkflowUID != "" {
							found = true
						}
					}
					if !found {
						t.Fatalf("specific missing retry not retained: %+v", h.AttemptGaps)
					}
				}
				want := 5
				if mode == "unavailable-pods" {
					want = 4
				}
				if len(h.Observations) != want || steps[0].MemoryPeakBytes.Count != want {
					t.Fatal("missing attempt acquired fabricated metrics")
				}
			} else if h.SafetyIncomplete || steps[0].Suggested == nil {
				t.Fatalf("collector success control refused: %+v", steps[0])
			}
		})
	}
}
