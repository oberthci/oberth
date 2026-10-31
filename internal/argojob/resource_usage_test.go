package argojob

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/resourcesampler"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

type resourceRecorder struct {
	mu     sync.Mutex
	values []model.StepResourceUsage
	notify chan struct{}
}

func (r *resourceRecorder) RecordStepResourceUsage(_ context.Context, values []model.StepResourceUsage) error {
	r.mu.Lock()
	r.values = append(r.values, values...)
	r.mu.Unlock()
	if r.notify != nil {
		select {
		case r.notify <- struct{}{}:
		default:
		}
	}
	return nil
}

type resourceSource struct {
	calls int
	fail  bool
}

func (*resourceSource) Interval() time.Duration { return time.Second }
func (s *resourceSource) Snapshot(context.Context, string) (resourcesampler.Snapshot, error) {
	s.calls++
	if s.fail {
		return resourcesampler.Snapshot{}, errors.New("unavailable")
	}
	return resourcesampler.Snapshot{}, nil
}
func TestResourceSamplerRetainsKilledUnknownOutsideTerminationMessage(t *testing.T) {
	wf, _, pod, _ := diagnosticFixture()
	pod.Spec.NodeName = "node-a"
	pod.Status.ContainerStatuses[0].State.Terminated.Message = `{"memory_peak_bytes":99999999,"io_read_bytes":123}`
	recorder := &resourceRecorder{}
	source := &resourceSource{fail: true}
	c := &Controller{kube: fake.NewClientset(pod), config: Config{Namespace: wf.Namespace}, resourceSource: source, resourceRecorder: recorder}
	if err := c.sampleResourceUsage(context.Background(), wf, wf.Annotations[runIDAnnotation]); err != nil {
		t.Fatal(err)
	}
	if len(recorder.values) != 1 || recorder.values[0].Reason != "OOMKilled" || recorder.values[0].Samples != 0 || recorder.values[0].MemoryPeakBytes != nil || recorder.values[0].IOReadBytes != nil {
		t.Fatalf("self-report trusted or killed unknown dropped %#v", recorder.values)
	}
	// A foreign Pod never triggers a node read, even if its labels match.
	pod.OwnerReferences[0].UID = "foreign"
	c.kube = fake.NewClientset(pod)
	source.calls = 0
	if err := c.sampleResourceUsage(context.Background(), wf, wf.Annotations[runIDAnnotation]); err != nil || source.calls != 0 {
		t.Fatal("foreign pod sampled")
	}
}
func TestResourceSamplerSessionRequiresVerifiedUpdateAndJoins(t *testing.T) {
	wf, _, pod, _ := diagnosticFixture()
	pod.Spec.NodeName = "node-a"
	recorder := &resourceRecorder{notify: make(chan struct{}, 4)}
	c := &Controller{kube: fake.NewClientset(pod), config: Config{Namespace: wf.Namespace}, resourceSource: &resourceSource{}, resourceRecorder: recorder}
	ctx, cancel := context.WithCancel(context.Background())
	session := c.resourceSession(ctx, wf.Annotations[runIDAnnotation])
	session.update(wf)
	select {
	case <-recorder.notify:
	case <-time.After(5 * time.Second):
		t.Fatal("sampler did not persist killed observation")
	}
	replacement := wf.DeepCopy()
	replacement.UID = "33333333-3333-4333-8333-333333333333"
	session.update(replacement)
	session.mu.Lock()
	uid := session.latest.UID
	session.mu.Unlock()
	if uid != wf.UID {
		t.Fatal("workflow incarnation changed")
	}
	cancel()
	session.finish()
	select {
	case <-session.done:
	default:
		t.Fatal("sampler leaked after Wait")
	}
}
func TestResourceSamplerRefusesWrongRunAndOversizeWorkflow(t *testing.T) {
	wf, _, pod, _ := diagnosticFixture()
	pod.Spec.NodeName = "node-a"
	source := &resourceSource{}
	recorder := &resourceRecorder{}
	c := &Controller{kube: fake.NewClientset(pod), resourceSource: source, resourceRecorder: recorder}
	if err := c.sampleResourceUsage(context.Background(), wf, "other"); err != nil || source.calls != 0 || len(recorder.values) != 0 {
		t.Fatal("wrong run sampled")
	}
	wf.Status.Nodes = make(wfv1.Nodes, 2049)
	for i := 0; i < 2049; i++ {
		wf.Status.Nodes[string(rune(i))] = wfv1.NodeStatus{}
	}
	if err := c.sampleResourceUsage(context.Background(), wf, wf.Annotations[runIDAnnotation]); err != nil || source.calls != 0 {
		t.Fatal("oversized workflow not bounded")
	}
}

func TestResourceSessionCopiesOnlyAtSamplingBoundary(t *testing.T) {
	wf, _, _, _ := diagnosticFixture()
	s := &resourceSession{wake: make(chan struct{}, 1)}
	s.update(wf)
	if s.latest != wf {
		t.Fatal("500ms poll still deep-copies the immutable Workflow")
	}
	snapshot := s.snapshot()
	if snapshot == wf || snapshot == nil {
		t.Fatal("sampling did not make its own snapshot")
	}
	snapshot.Annotations[runIDAnnotation] = "changed"
	if wf.Annotations[runIDAnnotation] == "changed" {
		t.Fatal("sampling snapshot aliases the controller observation")
	}
	newer := wf.DeepCopy()
	newer.Status.Message = "next poll"
	s.update(newer)
	if s.latest != newer || snapshot.Status.Message == "next poll" {
		t.Fatal("snapshot did not retain its original immutable observation")
	}
}

func TestResourceSizeComparisonUsesWorkflowNotTerminationClaims(t *testing.T) {
	wf, _, pod, _ := diagnosticFixture()
	pod.Spec.NodeName = "node-a"
	wf.Annotations[argoworkflow.SizeAnnotation] = "XL"
	pod.Status.ContainerStatuses[0].State.Terminated.Message = `{"workflow_size":"S","memory_peak_bytes":1}`
	beforeWorkflow, _ := json.Marshal(wf.Spec)
	beforePod, _ := json.Marshal(pod.Spec)
	recorder := &resourceRecorder{}
	kube := fake.NewClientset(pod)
	c := &Controller{kube: kube, config: Config{Namespace: wf.Namespace}, resourceSource: &resourceSource{fail: true}, resourceRecorder: recorder}
	if err := c.sampleResourceUsage(t.Context(), wf, wf.Annotations[runIDAnnotation]); err != nil {
		t.Fatal(err)
	}
	if len(recorder.values) != 1 || recorder.values[0].WorkflowSize != "XL" || recorder.values[0].MemoryPeakBytes != nil {
		t.Fatal("termination claim became report authority", recorder.values)
	}
	afterWorkflow, _ := json.Marshal(wf.Spec)
	afterPod, _ := json.Marshal(pod.Spec)
	if string(beforeWorkflow) != string(afterWorkflow) || string(beforePod) != string(afterPod) {
		t.Fatal("reporting changed authored resource requests/limits")
	}
	for _, action := range kube.Actions() {
		if action.GetVerb() != "list" {
			t.Fatal("report sampler mutated Kubernetes", action)
		}
	}
}

func TestResourceSizeComparisonTriggerClassOrUnknown(t *testing.T) {
	wf, _, _, _ := diagnosticFixture()
	wf.Annotations[argoworkflow.SizeAnnotation] = "L"
	wf.Annotations[argoworkflow.SizeByTriggerAnnotation] = "branch=M,promotion=XL"
	wf.Spec.Arguments.Parameters = nil
	if size := observedResourceWorkflowSize(wf); size != "" {
		t.Fatal("missing trigger was guessed", size)
	}
	wf.Spec.Arguments.Parameters = []wfv1.Parameter{{Name: argoworkflow.TriggerParameter, Value: wfv1.AnyStringPtr("promotion")}}
	if size := observedResourceWorkflowSize(wf); size != "XL" {
		t.Fatal("bound promotion class ignored", size)
	}
	wf.Spec.Arguments.Parameters = append(wf.Spec.Arguments.Parameters, wf.Spec.Arguments.Parameters[0])
	if size := observedResourceWorkflowSize(wf); size != "" {
		t.Fatal("ambiguous trigger was guessed", size)
	}
	wf.Spec.Arguments.Parameters = nil
	delete(wf.Annotations, argoworkflow.SizeByTriggerAnnotation)
	wf.Annotations[argoworkflow.SizeAnnotation] = "invalid"
	if size := observedResourceWorkflowSize(wf); size != "" {
		t.Fatal("invalid class accepted", size)
	}
	delete(wf.Annotations, argoworkflow.SizeAnnotation)
	if size := observedResourceWorkflowSize(wf); size != "M" {
		t.Fatal("valid Workflow default not respected", size)
	}
}

func TestResourceScrapeFailureDoesNotChangeWaitOrMutateWorkload(t *testing.T) {
	for _, succeeded := range []bool{false, true} {
		t.Run(fmt.Sprint("succeeded=", succeeded), func(t *testing.T) {
			wf, node, pod, _ := diagnosticFixture()
			pod.Spec.NodeName = "node-a"
			if succeeded {
				wf.Status.Phase = wfv1.WorkflowSucceeded
				node.Phase = wfv1.NodeSucceeded
				code := "0"
				node.Outputs.ExitCode = &code
				wf.Status.Nodes[node.ID] = node
				pod.Status.ContainerStatuses[0].State.Terminated.ExitCode = 0
				pod.Status.ContainerStatuses[0].State.Terminated.Reason = "Completed"
			}
			before := wf.DeepCopy()
			kube := fake.NewClientset(pod)
			controller := &Controller{
				workflows: &staticWorkflowClient{workflow: wf}, kube: kube,
				config: Config{Namespace: wf.Namespace, MaxRunLogBytes: 1 << 20}, interval: time.Millisecond,
				logs: func(context.Context, string, string, string) (io.ReadCloser, error) {
					return io.NopCloser(strings.NewReader("step output\n")), nil
				},
			}
			baseline, err := controller.Wait(t.Context(), wf.Name, wf.Annotations[runIDAnnotation], io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			kube.ClearActions()
			source := &resourceSource{fail: true}
			recorder := &resourceRecorder{}
			controller.WithResourceSampler(source, recorder)
			measured, err := controller.Wait(t.Context(), wf.Name, wf.Annotations[runIDAnnotation], io.Discard)
			if err != nil || measured.Succeeded != succeeded {
				t.Fatalf("sampler changed verdict: %+v %v", measured, err)
			}
			// Local collection timestamps/durations differ between Wait calls; execution
			// evidence and verdict fields must be identical with or without the sampler.
			for _, result := range []*Completion{&baseline, &measured} {
				for i := range result.Steps {
					result.Steps[i].Timing = nil
					if result.Steps[i].Termination != nil {
						result.Steps[i].Termination.ObservedAt = nil
					}
				}
			}
			if !reflect.DeepEqual(baseline, measured) {
				t.Fatalf("sampler failure changed completion: baseline=%+v measured=%+v", baseline, measured)
			}
			if source.calls == 0 {
				t.Fatal("test never reached failed scrape")
			}
			if !reflect.DeepEqual(wf, before) {
				t.Fatal("sampler changed Workflow")
			}
			for _, action := range kube.Actions() {
				if action.GetVerb() != "list" && action.GetVerb() != "get" {
					t.Fatalf("sampler failure mutated Kubernetes: %v", action)
				}
			}
			for _, row := range recorder.values {
				if row.Samples != 0 || row.MemoryPeakBytes != nil {
					t.Fatal("failed scrape manufactured metrics", row)
				}
			}
		})
	}
}
