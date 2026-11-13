package argojob

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
)

func TestPendingSeedReasonSurvivesAControllerRestart(t *testing.T) {
	config := seedTestConfig()
	name := sourceClaimName("wf")
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: config.Namespace, UID: "original", Labels: map[string]string{"oberth.ci/component": "source-seed"}}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: seedContainer}}, Volumes: []corev1.Volume{{Name: "source", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: name}}}}}, Status: corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{{Name: seedContainer, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "PRIVATE credentials"}}}}}}
	client := fake.NewClientset(pod)
	controller := &Controller{workflows: &schedulingWorkflowClient{err: apierrors.NewNotFound(schema.GroupResource{Resource: "workflows"}, "wf")}, seeder: &SourceSeeder{client: client, config: config}, config: config}
	got, err := controller.ObserveScheduling(t.Context(), "wf", "run", "sha")
	if err != nil || got == nil || got.Reason != "SourceSeedPending" || !strings.Contains(got.Message, "ImagePullBackOff") || strings.Contains(got.Message, "PRIVATE") {
		t.Fatal("restart lost curated seed reason", got, err)
	}
	if len(client.Actions()) != 1 || client.Actions()[0].GetVerb() != "get" || client.Actions()[0].GetResource().Resource != "pods" {
		t.Fatal("observation mutated state", client.Actions())
	}
	if len(controller.seeder.observations) != 0 {
		t.Fatal("fallback retained an unbounded identity cache")
	}
	if err := client.CoreV1().Pods(config.Namespace).Delete(t.Context(), name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	got, err = controller.ObserveScheduling(t.Context(), "wf", "run", "sha")
	if err != nil || got.Reason == "SourceSeedMissing" {
		t.Fatal("absence after restart claimed a lost created Pod", got, err)
	}
}

func TestRestartSeedObservationRefusesOtherPodShapes(t *testing.T) {
	for _, mutate := range []func(*corev1.Pod){
		func(p *corev1.Pod) { p.UID = "" },
		func(p *corev1.Pod) { p.Labels["oberth.ci/component"] = "other" },
		func(p *corev1.Pod) { p.Spec.Volumes[0].PersistentVolumeClaim.ClaimName = "other" },
		func(p *corev1.Pod) { p.Spec.Containers[0].Name = "other" },
	} {
		config := seedTestConfig()
		name := sourceClaimName("wf")
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: config.Namespace, UID: "uid", Labels: map[string]string{"oberth.ci/component": "source-seed"}}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: seedContainer}}, Volumes: []corev1.Volume{{Name: "source", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: name}}}}}}
		mutate(pod)
		seeder := &SourceSeeder{client: fake.NewClientset(pod), config: config}
		if _, err := seeder.observeScheduling(t.Context(), "wf", "run", "sha"); err == nil {
			t.Fatal("foreign Pod projected as this source seed")
		}
	}
}
