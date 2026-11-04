package argojob

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/oberthci/oberth/internal/model"
)

type dispositionRecorder struct {
	resourceRecorder
	dispositions []model.PodResourceDisposition
}

func (r *dispositionRecorder) RecordPodResourceDisposition(_ context.Context, f model.PodResourceDisposition) error {
	r.dispositions = append(r.dispositions, f)
	return nil
}

func TestResourceDispositionSurvivesAbsentMainRuntimeIdentity(t *testing.T) {
	wf, node, pod, _ := diagnosticFixture()
	node.TemplateName = "compile"
	wf.Status.Nodes[node.ID] = node
	pod.Name = PodName(wf, node)
	pod.Spec.NodeName = "node-a"
	pod.Status.Phase = corev1.PodFailed
	pod.Status.Reason = "Evicted"
	for i := range pod.Status.ContainerStatuses {
		pod.Status.ContainerStatuses[i].ContainerID = ""
	}
	recorder := &dispositionRecorder{}
	c := &Controller{kube: fake.NewClientset(pod), config: Config{Namespace: wf.Namespace}, resourceSource: &resourceSource{fail: true}, resourceRecorder: recorder}
	if err := c.sampleResourceUsage(t.Context(), wf, wf.Annotations[runIDAnnotation]); err != nil {
		t.Fatal(err)
	}
	if len(recorder.dispositions) != 1 || recorder.dispositions[0].PodUID != string(pod.UID) || recorder.dispositions[0].Reason != "Evicted" || len(recorder.values) != 0 {
		t.Fatalf("failure lost or fake metrics created: %+v %+v", recorder.dispositions, recorder.values)
	}
	pod.OwnerReferences[0].UID = "foreign"
	c.kube = fake.NewClientset(pod)
	if err := c.sampleResourceUsage(t.Context(), wf, wf.Annotations[runIDAnnotation]); err != nil {
		t.Fatal(err)
	}
	if len(recorder.dispositions) != 1 {
		t.Fatal("foreign Pod supplied disposition")
	}
}
