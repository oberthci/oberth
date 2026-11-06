package argojob

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/resourcesampler"
)

type retryResourceRecorder struct {
	resourceRecorder
	fail  bool
	calls int
}

func (r *retryResourceRecorder) RecordStepResourceUsage(ctx context.Context, values []model.StepResourceUsage) error {
	r.calls++
	if r.fail {
		return errors.New("unavailable")
	}
	return r.resourceRecorder.RecordStepResourceUsage(ctx, values)
}

func TestResourceSessionSkipsFinishedOnlyAfterSuccessfulRecording(t *testing.T) {
	wf, _, pod, _ := diagnosticFixture()
	pod.Spec.NodeName = "node-a"
	source, recorder := &resourceSource{}, &retryResourceRecorder{fail: true}
	c := &Controller{kube: fake.NewClientset(pod), resourceSource: source, resourceRecorder: recorder}
	finished := map[string]time.Time{}
	if err := c.sampleResourceUsageWithState(t.Context(), wf, wf.Annotations[runIDAnnotation], finished); err == nil || len(finished) != 0 {
		t.Fatal("failed recording suppressed final evidence", err, finished)
	}
	recorder.fail = false
	if err := c.sampleResourceUsageWithState(t.Context(), wf, wf.Annotations[runIDAnnotation], finished); err != nil || len(finished) != 1 {
		t.Fatal("finish not cached", err, finished)
	}
	if err := c.sampleResourceUsageWithState(t.Context(), wf, wf.Annotations[runIDAnnotation], finished); err != nil {
		t.Fatal(err)
	}
	if recorder.calls != 2 || source.calls != 2 || len(recorder.values) != 1 {
		t.Fatal("finished containers reached recorder/scraper again", recorder.calls, source.calls)
	}
	// A long run may have more lifetime IDs than durable row capacity. The
	// session's suppression set must stay bounded across later passes too.
	finished = map[string]time.Time{}
	for i := 0; i < 4096; i++ {
		finished[fmt.Sprint(i)] = time.Time{}
	}
	if err := c.sampleResourceUsageWithState(t.Context(), wf, wf.Annotations[runIDAnnotation], finished); err != nil || len(finished) != 4096 {
		t.Fatal("finished-ID cache exceeded bound", len(finished), err)
	}
}

type panicResourceSource struct{}

func (panicResourceSource) Interval() time.Duration { return time.Second }
func (panicResourceSource) Snapshot(context.Context, string) (resourcesampler.Snapshot, error) {
	panic("sampler panic canary")
}

func TestResourceSessionContainsTelemetryPanic(t *testing.T) {
	wf, _, pod, _ := diagnosticFixture()
	pod.Spec.NodeName = "node-a"
	c := &Controller{kube: fake.NewClientset(pod), resourceSource: panicResourceSource{}, resourceRecorder: &resourceRecorder{}}
	session := c.resourceSession(t.Context(), wf.Annotations[runIDAnnotation])
	session.update(wf)
	select {
	case <-session.done:
	case <-time.After(5 * time.Second):
		t.Fatal("panicking sampler did not stop/join")
	}
	session.finish()
}

func TestResourceCandidateCapsWorkflowPass(t *testing.T) {
	for _, launcherMode := range []bool{false} {
		t.Run(fmt.Sprint("launcher=", launcherMode), func(t *testing.T) {
			wf, node, base, at := diagnosticFixture()
			base.Spec.NodeName = "node-a"
			objects := []runtime.Object{}
			wf.Status.Nodes = wfv1.Nodes{}

			for i := 0; i < 33; i++ {
				p := base.DeepCopy()
				p.UID = types.UID(fmt.Sprintf("%08x-2222-4222-8222-222222222222", i+1))
				p.Spec.Containers = nil
				p.Status.ContainerStatuses = nil
				{
					n := node
					n.ID = fmt.Sprintf("workflow-test-%d", i)
					n.Name = fmt.Sprintf("workflow-test.step%d", i)
					n.DisplayName = fmt.Sprintf("step%d", i)
					wf.Status.Nodes[n.ID] = n
					p.Name = PodName(wf, n)
					p.Annotations = map[string]string{"workflows.argoproj.io/node-id": n.ID, "workflows.argoproj.io/node-name": n.Name}
				}
				for j := 0; j < 128; j++ {
					name := fmt.Sprintf("c%d", j)
					p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: name})
					p.Status.ContainerStatuses = append(p.Status.ContainerStatuses, corev1.ContainerStatus{Name: name, ContainerID: fmt.Sprintf("containerd://%064x", i*128+j+1), State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(at.Add(-time.Minute))}}})
				}
				objects = append(objects, p)
			}
			source, recorder := &resourceSource{}, &resourceRecorder{}
			c := &Controller{kube: fake.NewClientset(objects...), resourceSource: source, resourceRecorder: recorder}
			if err := c.sampleResourceUsage(t.Context(), wf, wf.Annotations[runIDAnnotation]); err != nil {
				t.Fatal(err)
			}
			if len(recorder.values) < 3500 || len(recorder.values) > 4096 {
				t.Fatal("candidate cap missing or fixture not exercised", len(recorder.values))
			}
			if source.calls != 1 {
				t.Fatal("node reads not coalesced", source.calls)
			}
		})
	}
}

func TestResourceSessionRecordsLatePodFinishOnce(t *testing.T) {
	wf, _, pod, _ := diagnosticFixture()
	pod.Spec.NodeName = "node-a"
	started := pod.Status.ContainerStatuses[0].State.Terminated.StartedAt
	pod.Status.StartTime = &started
	pod.Status.Phase = corev1.PodRunning
	pod.Status.ContainerStatuses[1].ContainerID = "containerd://" + strings.Repeat("c", 64)
	pod.Status.ContainerStatuses[1].State.Terminated.StartedAt = started
	pod.Status.ContainerStatuses[1].State.Terminated.FinishedAt = pod.Status.ContainerStatuses[0].State.Terminated.FinishedAt
	source, recorder := &resourceSource{}, &resourceRecorder{}
	c := &Controller{kube: fake.NewClientset(pod), resourceSource: source, resourceRecorder: recorder}
	finished := map[string]time.Time{}
	if err := c.sampleResourceUsageWithState(t.Context(), wf, wf.Annotations[runIDAnnotation], finished); err != nil {
		t.Fatal(err)
	}
	if len(recorder.values) != 2 || !recorder.values[0].PodFinishedAt.IsZero() {
		t.Fatal("fixture did not precede terminal phase", recorder.values)
	}
	// Exercise the same transition with the bounded cache already full.
	for len(finished) < 4096 {
		finished[fmt.Sprint(len(finished))] = time.Time{}
	}
	pod.Status.Phase = corev1.PodFailed
	c.kube = fake.NewClientset(pod)
	if err := c.sampleResourceUsageWithState(t.Context(), wf, wf.Annotations[runIDAnnotation], finished); err != nil {
		t.Fatal(err)
	}
	if len(recorder.values) != 4 || recorder.values[2].PodFinishedAt.IsZero() || recorder.values[3].PodFinishedAt.IsZero() {
		t.Fatal("suppression lost late pod finish", recorder.values)
	}
	if err := c.sampleResourceUsageWithState(t.Context(), wf, wf.Annotations[runIDAnnotation], finished); err != nil {
		t.Fatal(err)
	}
	if len(recorder.values) != 4 || source.calls != 2 || len(finished) != 4096 {
		t.Fatal("pod finish update did not return to suppression", len(recorder.values), source.calls, len(finished))
	}
}
