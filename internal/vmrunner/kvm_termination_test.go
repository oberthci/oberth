package vmrunner

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func terminationFixture() (KVMTerminationBinding, *corev1.Pod) {
	start := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	binding := KVMTerminationBinding{Namespace: "pipeline", WorkflowName: "run", WorkflowUID: "workflow-uid", PodName: "trusted-test", PodUID: "pod-uid", ContainerID: "containerd://container", HarnessImageRef: "registry.example/harness@sha256:" + strings.Repeat("c", 64), AdmittedAt: start, Deadline: start.Add(time.Minute)}
	controller := true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: binding.PodName, Namespace: binding.Namespace, UID: types.UID(binding.PodUID), OwnerReferences: []metav1.OwnerReference{{APIVersion: "argoproj.io/v1alpha1", Kind: "Workflow", Name: binding.WorkflowName, UID: types.UID(binding.WorkflowUID), Controller: &controller}}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: binding.HarnessImageRef}}},
		Status:     corev1.PodStatus{Phase: corev1.PodSucceeded, ContainerStatuses: []corev1.ContainerStatus{{Name: "main", ContainerID: binding.ContainerID, ImageID: binding.HarnessImageRef, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ContainerID: binding.ContainerID, StartedAt: metav1.NewTime(start.Add(time.Second)), FinishedAt: metav1.NewTime(start.Add(30 * time.Second))}}}}},
	}
	return binding, pod
}

func TestKVMTerminationRequiresBoundCurrentProcess(t *testing.T) {
	binding, pod := terminationFixture()
	if err := VerifyKVMTermination(binding, pod); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*KVMTerminationBinding, *corev1.Pod){
		"empty binding":              func(b *KVMTerminationBinding, _ *corev1.Pod) { *b = KVMTerminationBinding{} },
		"missing expected container": func(b *KVMTerminationBinding, _ *corev1.Pod) { b.ContainerID = "" },
		"missing expected pod":       func(b *KVMTerminationBinding, _ *corev1.Pod) { b.PodUID = "" },
		"wrong pod":                  func(_ *KVMTerminationBinding, p *corev1.Pod) { p.UID = "substitution" },
		"wrong workflow":             func(_ *KVMTerminationBinding, p *corev1.Pod) { p.OwnerReferences[0].UID = "substitution" },
		"no owner":                   func(_ *KVMTerminationBinding, p *corev1.Pod) { p.OwnerReferences = nil },
		"wrong owner kind":           func(_ *KVMTerminationBinding, p *corev1.Pod) { p.OwnerReferences[0].Kind = "Job" },
		"owner not controller":       func(_ *KVMTerminationBinding, p *corev1.Pod) { p.OwnerReferences[0].Controller = nil },
		"running pod":                func(_ *KVMTerminationBinding, p *corev1.Pod) { p.Status.Phase = corev1.PodRunning },
		"wrong image": func(_ *KVMTerminationBinding, p *corev1.Pod) {
			p.Spec.Containers[0].Image = "registry.example/other:latest"
		},
		"tag only binding":     func(b *KVMTerminationBinding, _ *corev1.Pod) { b.HarnessImageRef = "registry.example/harness:latest" },
		"missing actual image": func(_ *KVMTerminationBinding, p *corev1.Pod) { p.Status.ContainerStatuses[0].ImageID = "" },
		"digest suffix":        func(_ *KVMTerminationBinding, p *corev1.Pod) { p.Status.ContainerStatuses[0].ImageID += "0" },
		"digest prefix": func(_ *KVMTerminationBinding, p *corev1.Pod) {
			p.Status.ContainerStatuses[0].ImageID = "junk://" + p.Status.ContainerStatuses[0].ImageID
		},
		"wrong actual container": func(_ *KVMTerminationBinding, p *corev1.Pod) {
			p.Status.ContainerStatuses[0].ContainerID = "containerd://different"
		},
		"wrong terminated container": func(_ *KVMTerminationBinding, p *corev1.Pod) {
			p.Status.ContainerStatuses[0].State.Terminated.ContainerID = "containerd://different"
		},
		"restart":           func(_ *KVMTerminationBinding, p *corev1.Pod) { p.Status.ContainerStatuses[0].RestartCount = 1 },
		"negative restarts": func(_ *KVMTerminationBinding, p *corev1.Pod) { p.Status.ContainerStatuses[0].RestartCount = -1 },
		"old termination": func(_ *KVMTerminationBinding, p *corev1.Pod) {
			s := &p.Status.ContainerStatuses[0]
			s.LastTerminationState = s.State
			s.State = corev1.ContainerState{}
		},
		"prior termination": func(_ *KVMTerminationBinding, p *corev1.Pod) {
			s := &p.Status.ContainerStatuses[0]
			s.LastTerminationState = s.State
		},
		"PASS then exit 1": func(_ *KVMTerminationBinding, p *corev1.Pod) {
			s := p.Status.ContainerStatuses[0].State.Terminated
			s.Message = "PASS"
			s.ExitCode = 1
		},
		"signal": func(_ *KVMTerminationBinding, p *corev1.Pod) {
			p.Status.ContainerStatuses[0].State.Terminated.Signal = 9
		},
		"missing start": func(_ *KVMTerminationBinding, p *corev1.Pod) {
			p.Status.ContainerStatuses[0].State.Terminated.StartedAt = metav1.Time{}
		},
		"missing finish": func(_ *KVMTerminationBinding, p *corev1.Pod) {
			p.Status.ContainerStatuses[0].State.Terminated.FinishedAt = metav1.Time{}
		},
		"finish before start": func(b *KVMTerminationBinding, p *corev1.Pod) {
			p.Status.ContainerStatuses[0].State.Terminated.FinishedAt = metav1.NewTime(b.AdmittedAt)
		},
		"start before admission": func(b *KVMTerminationBinding, p *corev1.Pod) {
			p.Status.ContainerStatuses[0].State.Terminated.StartedAt = metav1.NewTime(b.AdmittedAt.Add(-time.Second))
		},
		"timeout": func(b *KVMTerminationBinding, p *corev1.Pod) {
			p.Status.ContainerStatuses[0].State.Terminated.FinishedAt = metav1.NewTime(b.Deadline.Add(time.Second))
		},
		"missing deadline": func(b *KVMTerminationBinding, _ *corev1.Pod) { b.Deadline = time.Time{} },
		"unbounded deadline": func(b *KVMTerminationBinding, _ *corev1.Pod) {
			b.Deadline = b.AdmittedAt.Add(MaxDeadline + time.Second)
		},
		"duplicate status": func(_ *KVMTerminationBinding, p *corev1.Pod) {
			p.Status.ContainerStatuses = append(p.Status.ContainerStatuses, p.Status.ContainerStatuses[0])
		},
	} {
		t.Run(name, func(t *testing.T) {
			b, p := terminationFixture()
			mutate(&b, p)
			if err := VerifyKVMTermination(b, p); err == nil {
				t.Fatal("invalid process proof accepted")
			}
		})
	}
}

func TestKVMRuntimeImageDigestEncodings(t *testing.T) {
	binding, pod := terminationFixture()
	digest := "sha256:" + strings.Repeat("c", 64)
	for _, id := range []string{digest, "docker://" + digest, "docker-pullable://" + binding.HarnessImageRef, binding.HarnessImageRef} {
		pod.Status.ContainerStatuses[0].ImageID = id
		if err := VerifyKVMTermination(binding, pod); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
	}
}
