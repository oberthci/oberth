package argojob

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
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
		{name: "cached-completed", cached: wfv1.WorkflowSucceeded, live: wfv1.WorkflowFailed, reads: 1},
		{name: "cached-completed-now-running", cached: wfv1.WorkflowSucceeded, live: wfv1.WorkflowRunning, reads: 1},
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
			if found && tc.live.Completed() && tc.reads != 0 {
				if binding.Terminal == nil || !binding.Terminal.Terminal || binding.Terminal.Succeeded != (tc.live == wfv1.WorkflowSucceeded) {
					t.Fatalf("terminal result was not live: %+v", binding.Terminal)
				}
			} else if binding.Terminal != nil {
				t.Fatal("running/absent Workflow produced terminal result")
			}
			if !reflect.DeepEqual(cached, before) {
				t.Fatal("shared cached object was mutated")
			}
		})
	}
}

func TestStartupAdoptionRetainsUnknownAndRejectsUIDSubstitution(t *testing.T) {
	for _, mode := range []string{"unhealthy", "live-unavailable", "changed-uid"} {
		t.Run(mode, func(t *testing.T) {
			wf, request := adoptionWorkflow()
			wf.Status.Phase = wfv1.WorkflowSucceeded
			client := &adoptionLiveClient{staticWorkflowClient: staticWorkflowClient{workflow: wf.DeepCopy()}}
			cache := adoptionCache{workflow: wf, healthy: mode != "unhealthy"}
			if mode == "live-unavailable" {
				client.readErr = errors.New("API unavailable")
			}
			if mode == "changed-uid" {
				client.workflow.UID = "replacement-uid"
			}
			controller := &Controller{workflows: client, config: Config{Namespace: wf.Namespace}, hub: cache}
			binding, found, err := controller.Adopt(t.Context(), request)
			if err == nil || found || binding.Terminal != nil {
				t.Fatal("uncertain/foreign Workflow adopted", binding, found, err)
			}
			if errors.Is(err, ErrAdoptionRefused) != (mode == "changed-uid") {
				t.Fatal("outage became a binding refusal", err)
			}
			if mode == "unhealthy" && client.reads != 0 {
				t.Fatal("unhealthy cache caused per-run API reads")
			}
		})
	}
}
