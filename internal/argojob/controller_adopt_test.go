package argojob

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

type adoptionCache struct {
	workflow *wfv1.Workflow
	healthy  bool
}

func TestStartupAdoptionWaitsThroughCapabilityAPIOutage(t *testing.T) {
	grant := operatorCapability()
	submission := requestWithCapability(t, operatorResourceDocument, grant)
	workflow, err := Build(testConfig(), submission)
	if err != nil {
		t.Fatal(err)
	}
	workflow.UID = "operator-workflow-uid"
	workflow.Status.Phase = wfv1.WorkflowRunning
	kube := operatorKubeClient(grant)
	reads := 0
	kube.PrependReactor("get", "serviceaccounts", func(clienttesting.Action) (bool, runtime.Object, error) {
		reads++
		if reads == 1 {
			return true, nil, apierrors.NewServiceUnavailable("API unavailable")
		}
		return false, nil, nil
	})
	client := &adoptionLiveClient{readErr: errors.New("cached running adoption made a Workflow GET")}
	controller := &Controller{kube: kube, workflows: client, config: testConfig(), hub: adoptionCache{workflow: workflow, healthy: true}}
	bindCapabilityLoader(t, controller, func(context.Context, int64) (*argoworkflow.Capability, error) { return grant, nil })
	request := AdoptionRequest{Name: submission.Name, RunID: submission.RunID, Ref: submission.Ref, SHA: submission.SHA, Trigger: submission.Trigger, RunTrigger: argoworkflow.RunTriggerBranch, RepositoryID: submission.RepositoryID}
	if _, found, err := controller.Adopt(t.Context(), request); err == nil || found || errors.Is(err, ErrAdoptionRefused) {
		t.Fatal("unavailable capability read changed adoption classification", found, err)
	}
	if _, found, err := controller.Adopt(t.Context(), request); err != nil || !found || client.reads != 0 || reads != 2 {
		t.Fatal("capability recovery changed Workflow observation", found, err, client.reads, reads)
	}
}

func (h adoptionCache) Workflow(name string) (*wfv1.Workflow, bool) {
	return h.workflow, h.workflow != nil && h.workflow.Name == name
}
func (adoptionCache) PodsOf(string) []*corev1.Pod         { return nil }
func (adoptionCache) Pods() []*corev1.Pod                 { return nil }
func (h adoptionCache) Healthy() bool                     { return h.healthy }
func (adoptionCache) Synced() bool                        { return true }
func (h adoptionCache) UnhealthySince() (time.Time, bool) { return time.Time{}, !h.healthy }

type adoptionLiveClient struct {
	staticWorkflowClient
	reads   int
	readErr error
}

func (c *adoptionLiveClient) Get(_ context.Context, name string, _ metav1.GetOptions) (*wfv1.Workflow, error) {
	c.reads++
	if c.readErr != nil {
		return nil, c.readErr
	}
	if c.workflow == nil {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: "argoproj.io", Resource: "workflows"}, name)
	}
	return c.workflow.DeepCopy(), nil
}
func (*adoptionLiveClient) Create(context.Context, *wfv1.Workflow, metav1.CreateOptions) (*wfv1.Workflow, error) {
	panic("startup adoption submitted a Workflow")
}
func (*adoptionLiveClient) Delete(context.Context, string, metav1.DeleteOptions) error {
	panic("startup adoption deleted a Workflow")
}

func TestStartupAdoptionUsesCacheAndLiveConfirmation(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		cached, live              wfv1.WorkflowPhase
		miss, absent, unavailable bool
		reads                     int
	}{
		{name: "cached-running", cached: wfv1.WorkflowRunning, unavailable: true, reads: 0},
		{name: "cached-completed", cached: wfv1.WorkflowSucceeded, live: wfv1.WorkflowFailed, reads: 0},
		{name: "cached-completed-now-running", cached: wfv1.WorkflowSucceeded, live: wfv1.WorkflowRunning, reads: 0},
		{name: "miss-confirmed-absent", miss: true, absent: true, reads: 1},
		{name: "miss-live-running", miss: true, live: wfv1.WorkflowRunning, reads: 1},
		{name: "miss-live-completed", miss: true, live: wfv1.WorkflowSucceeded, reads: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cached, request := adoptionWorkflow()
			cached.Status.Phase = tc.cached
			before := cached.DeepCopy()
			live := cached.DeepCopy()
			live.Status.Phase = tc.live
			client := &adoptionLiveClient{staticWorkflowClient: staticWorkflowClient{workflow: live}}
			if tc.absent {
				client.workflow = nil
			}
			if tc.unavailable {
				client.readErr = errors.New("API unavailable")
			}
			cache := adoptionCache{workflow: cached, healthy: true}
			if tc.miss {
				cache.workflow = nil
			}
			controller := &Controller{workflows: client, config: Config{Namespace: cached.Namespace}, hub: cache}
			binding, found, err := controller.Adopt(t.Context(), request)
			if err != nil || found == tc.absent || client.reads != tc.reads {
				t.Fatalf("adopt=%+v found=%v reads=%d err=%v", binding, found, client.reads, err)
			}
			if found && binding.UID != cached.UID {
				t.Fatal("adoption lost exact cached UID", binding)
			}
			if !reflect.DeepEqual(cached, before) {
				t.Fatal("shared cached object was mutated")
			}
		})
	}
}

func TestStartupAdoptionRetainsUnknownAndRejectsUIDSubstitution(t *testing.T) {
	for _, mode := range []string{"unhealthy", "miss-live-unavailable", "changed-uid"} {
		t.Run(mode, func(t *testing.T) {
			wf, request := adoptionWorkflow()
			wf.Status.Phase = wfv1.WorkflowSucceeded
			client := &adoptionLiveClient{staticWorkflowClient: staticWorkflowClient{workflow: wf.DeepCopy()}}
			cache := adoptionCache{workflow: wf, healthy: mode != "unhealthy"}
			if mode == "miss-live-unavailable" {
				cache.workflow = nil
				client.readErr = apierrors.NewServiceUnavailable("API unavailable")
			}
			controller := &Controller{workflows: client, config: Config{Namespace: wf.Namespace, MaxRunLogBytes: 4096}, hub: cache, interval: time.Millisecond}
			binding, found, err := controller.Adopt(t.Context(), request)
			if mode == "changed-uid" {
				if err != nil || !found || client.reads != 0 {
					t.Fatal(binding, found, err, client.reads)
				}
				client.workflow.UID = "replacement-uid"
				if _, err := controller.WaitResumed(t.Context(), request.Name, request.RunID, io.Discard, nil, 0, binding); err == nil || !strings.Contains(err.Error(), "identity changed") {
					t.Fatal("replaced Workflow observed", err)
				}
			} else if err == nil || found || errors.Is(err, ErrAdoptionRefused) {
				t.Fatal("outage changed classification", binding, found, err)
			}
			if mode == "unhealthy" && client.reads != 0 {
				t.Fatal("unhealthy cache read live")
			}
		})
	}
}

func TestStartupAdoptionCompletedWaitPublishesLogsAndRetiresIdentity(t *testing.T) {
	wf, request := adoptionWorkflow()
	wf.Status.Phase = wfv1.WorkflowSucceeded
	config := testConfig()
	config.Namespace = wf.Namespace
	config.MaxRunLogBytes = 1 << 20
	kube := kubefake.NewClientset()
	client := &adoptionLiveClient{staticWorkflowClient: staticWorkflowClient{workflow: wf}}
	controller := &Controller{workflows: client, kube: kube, config: config, hub: adoptionCache{workflow: wf, healthy: true}, interval: time.Millisecond}
	kube.PrependReactor("create", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
		action.(clienttesting.CreateAction).GetObject().(*corev1.Pod).UID = "identity-lifetime-uid"
		return false, nil, nil
	})
	lifetime, err := controller.executorIdentityPod(t.Context(), wf, config.ExecutorServiceAccount)
	if err != nil {
		t.Fatal(err)
	}
	reads := 0
	controller.WithPodLogReader(func(context.Context, string, string, string) (io.ReadCloser, error) {
		reads++
		return io.NopCloser(strings.NewReader("finished during downtime\n")), nil
	})
	binding, found, err := controller.Adopt(t.Context(), request)
	if err != nil || !found || client.reads != 0 {
		t.Fatal(binding, found, err, client.reads)
	}
	var output bytes.Buffer
	result, err := controller.WaitResumed(t.Context(), wf.Name, request.RunID, &output, nil, 0, binding)
	if err != nil || !result.Succeeded || !strings.Contains(output.String(), "finished during downtime") || reads != len(StepResults(wf)) {
		t.Fatal(result, err, output.String(), reads)
	}
	if _, err := kube.CoreV1().Pods(wf.Namespace).Get(t.Context(), lifetime.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatal("completed adoption retained executor identity", err)
	}
}
