package argojob

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/resourcesampler"
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
