package argojob

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	"github.com/oberthci/oberth/internal/runlog"
)

type unhealthyLogCache struct{ containerLogCache }

func (unhealthyLogCache) Healthy() bool { return false }

func TestNativeFinalCaptureIncludesRunningContainers(t *testing.T) {
	for _, mode := range []string{"timeout", "cancel", "unhealthy"} {
		t.Run(mode, func(t *testing.T) {
			c, store, wf, pod := nativeContainerLogFixture(t)
			wf.Status.Phase = wfv1.WorkflowFailed
			pod.Status = corev1.PodStatus{Phase: corev1.PodRunning}
			c.idleInterval = time.Millisecond
			c.workflows = &staticWorkflowClient{workflow: wf}
			if mode == "timeout" {
				wf.Status.Message = "workflow deadline exceeded"
			}
			if mode == "cancel" {
				wf.Spec.Shutdown = wfv1.ShutdownStrategyStop
				wf.Status.Message = "Stopped with strategy 'Stop'"
			}
			if mode == "unhealthy" {
				c.hub = unhealthyLogCache{containerLogCache{workflow: wf, pods: []*corev1.Pod{pod}}}
				if err := store.TrackContainerLogs("run", []runlog.ContainerLog{containerLogEntry(StepContainers(wf, []*corev1.Pod{pod})[0])}); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			c.WithContainerLogSource(func(context.Context, string, string, corev1.PodLogOptions) (io.ReadCloser, error) {
				calls++
				return io.NopCloser(strings.NewReader("last output\n")), nil
			}, func(context.Context, string, string) (*corev1.Pod, error) { return pod.DeepCopy(), nil })
			if _, err := c.Wait(t.Context(), wf.Name, "run", io.Discard); err != nil {
				t.Fatal(err)
			}
			body, _, entry, err := store.ReadContainerLog(t.Context(), "run", "node", "main", runlog.Filter{})
			if err != nil || calls != 1 || !entry.Captured || !strings.Contains(string(body), "last output") {
				t.Fatal(string(body), entry, calls, err)
			}
		})
	}
}

func TestNativeSlowCaptureDoesNotDelayObservationOrRetirement(t *testing.T) {
	c, store, wf, pod := nativeContainerLogFixture(t)
	running := wf.DeepCopy()
	running.Status.Phase = wfv1.WorkflowRunning
	hub := newControlledWaitHub(running)
	hub.setPods(pod)
	c.hub = hub
	c.idleInterval = time.Millisecond
	opened := make(chan struct{})
	reader, writer := io.Pipe()
	defer writer.Close()
	c.WithContainerLogSource(func(context.Context, string, string, corev1.PodLogOptions) (io.ReadCloser, error) {
		close(opened)
		hub.setWorkflow(wf)
		return reader, nil
	}, func(context.Context, string, string) (*corev1.Pod, error) { return pod.DeepCopy(), nil })
	c.config.ExecutorServiceAccount = "executor"
	identity := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: c.identityPodName(wf.UID, "executor"), Namespace: wf.Namespace, Annotations: map[string]string{runIDAnnotation: "run"}, OwnerReferences: []metav1.OwnerReference{{APIVersion: "argoproj.io/v1alpha1", Kind: "Workflow", Name: wf.Name, UID: wf.UID, Controller: ptr.To(true)}}}}
	c.kube = fake.NewClientset(identity)
	retired := make(chan struct{}, 1)
	c.kube.(*fake.Clientset).PrependReactor("delete", "pods", func(action ktesting.Action) (bool, runtime.Object, error) {
		retired <- struct{}{}
		return false, nil, nil
	})
	c.workflows = &staticWorkflowClient{workflow: wf}
	done := make(chan error, 1)
	go func() { _, err := c.Wait(t.Context(), wf.Name, "run", io.Discard); done <- err }()
	select {
	case <-opened:
	case <-time.After(time.Second):
		t.Fatal("copy did not open")
	}
	select {
	case <-retired:
	case <-time.After(time.Second):
		_ = reader.Close()
		<-done
		t.Fatal("log read delayed completion or identity retirement")
	}
	if _, err := store.AppendRunLog("run", []byte("progress\n")); err != nil {
		t.Fatal(err)
	}
	_ = writer.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("copy task did not join")
	}
}

func TestNativeTTLDeletedWorkflowUsesObservedVerdict(t *testing.T) {
	c, _, wf, pod := nativeContainerLogFixture(t)
	wf.Spec.TTLStrategy = &wfv1.TTLStrategy{SecondsAfterCompletion: ptr.To(int32(0))}
	c.workflows = missingWorkflowClient{}
	c.idleInterval = time.Millisecond
	c.WithContainerLogSource(func(context.Context, string, string, corev1.PodLogOptions) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("completed\n")), nil
	}, func(context.Context, string, string) (*corev1.Pod, error) { return pod.DeepCopy(), nil })
	completion, err := c.Wait(t.Context(), wf.Name, "run", io.Discard)
	if err != nil || !completion.Succeeded {
		t.Fatal("cleanup changed observed verdict", completion, err)
	}
	// Without any terminal observation, native cleanup can erase the verdict.
	// Oberth admits it unchanged and reports the documented missing-result error.
	c.hub = containerLogCache{}
	_, err = c.Wait(t.Context(), wf.Name, "run", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "authored cleanup") {
		t.Fatal(err)
	}
}

func TestNativeCompletedAdoptionCapturesLogsAndRetiresIdentity(t *testing.T) {
	for _, stopped := range []bool{false, true} {
		t.Run(map[bool]string{false: "completed", true: "completed-Stop"}[stopped], func(t *testing.T) {
			wf, request := adoptionWorkflow()
			if stopped {
				wf.Status.Phase = wfv1.WorkflowFailed
				wf.Spec.Shutdown = wfv1.ShutdownStrategyStop
				wf.Status.Message = "Stopped with strategy 'Stop'"
			}
			config := testConfig()
			config.Namespace = wf.Namespace
			config.MaxRunLogBytes = 1 << 20
			kube := fake.NewClientset()
			store, err := runlog.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			var pods []*corev1.Pod
			for _, node := range wf.Status.Nodes {
				if node.Type != wfv1.NodeTypePod {
					continue
				}
				pods = append(pods, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "opaque-" + node.ID, Namespace: wf.Namespace, UID: types.UID(node.ID), Labels: map[string]string{"workflows.argoproj.io/workflow": wf.Name}, Annotations: map[string]string{"workflows.argoproj.io/node-id": node.ID, "workflows.argoproj.io/node-name": node.Name}, OwnerReferences: []metav1.OwnerReference{{APIVersion: "argoproj.io/v1alpha1", Kind: "Workflow", Name: wf.Name, UID: wf.UID, Controller: ptr.To(true)}}}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main"}}}, Status: corev1.PodStatus{Phase: corev1.PodSucceeded}})
			}
			c := &Controller{workflows: &staticWorkflowClient{workflow: wf}, kube: kube, config: config, hub: containerLogCache{workflow: wf, pods: pods}, containerLogs: store, idleInterval: time.Millisecond}
			kube.PrependReactor("create", "pods", func(action ktesting.Action) (bool, runtime.Object, error) {
				action.(ktesting.CreateAction).GetObject().(*corev1.Pod).UID = "identity-lifetime-uid"
				return false, nil, nil
			})
			lifetime, err := c.executorIdentityPod(t.Context(), wf, config.ExecutorServiceAccount)
			if err != nil {
				t.Fatal(err)
			}
			reads := 0
			c.WithContainerLogSource(func(context.Context, string, string, corev1.PodLogOptions) (io.ReadCloser, error) {
				reads++
				return io.NopCloser(strings.NewReader("finished during downtime\n")), nil
			}, func(_ context.Context, _, name string) (*corev1.Pod, error) {
				for _, pod := range pods {
					if pod.Name == name {
						return pod.DeepCopy(), nil
					}
				}
				return nil, apierrors.NewNotFound(corev1.Resource("pods"), name)
			})
			binding, found, err := c.Adopt(t.Context(), request)
			if err != nil || !found {
				t.Fatal(binding, found, err)
			}
			result, err := c.WaitResumed(t.Context(), wf.Name, request.RunID, io.Discard, binding)
			if err != nil || result.Succeeded == stopped || result.OperatorStopped != stopped || reads != len(pods) {
				t.Fatal(result, reads, err)
			}
			for _, pod := range pods {
				body, _, entry, err := store.ReadContainerLog(t.Context(), request.RunID, pod.Annotations["workflows.argoproj.io/node-id"], "main", runlog.Filter{})
				if err != nil || string(body) != "finished during downtime\n" || entry.Stage == "" || entry.Step == "" {
					t.Fatal(string(body), entry, err)
				}
				named, _, err := store.ReadFiltered(request.RunID, entry.Stage, entry.NodeID, runlog.Filter{})
				if err != nil || string(named) != string(body) {
					t.Fatal(string(named), err)
				}
			}
			if _, err := kube.CoreV1().Pods(wf.Namespace).Get(t.Context(), lifetime.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
				t.Fatal("completed adoption retained identity", err)
			}
		})
	}
}

func TestNativeCompletedUnobservedRunningNodeHasReason(t *testing.T) {
	c, store, wf, _ := nativeContainerLogFixture(t)
	node := wf.Status.Nodes["node"]
	node.Phase = wfv1.NodeRunning
	wf.Status.Nodes["node"] = node
	if err := c.ObserveContainerLogs(t.Context(), wf, "run", nil); err != nil {
		t.Fatal(err)
	}
	body, _, _, err := store.ReadContainerLog(t.Context(), "run", "node", "", runlog.Filter{})
	if err != nil || !strings.Contains(string(body), "not observed") {
		t.Fatal(string(body), err)
	}
}
