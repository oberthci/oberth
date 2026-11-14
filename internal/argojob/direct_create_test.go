package argojob

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/pipeline"
)

type createCache struct {
	testObservationHub
	workflows []*wfv1.Workflow
}

func (h *createCache) WorkflowsForRun(string) []*wfv1.Workflow { return h.workflows }

func TestDirectCreateSettlesFromRunLabelHubWithOneGETForMiss(t *testing.T) {
	for _, mode := range []string{"hub", "live", "missing", "unreachable", "foreign"} {
		t.Run(mode, func(t *testing.T) {
			config := seedTestConfig()
			request := testRequest(pipeline.TriggerCI, greedyDocument)
			intended, err := Build(config, request)
			if err != nil {
				t.Fatal(err)
			}
			client := &createRetryWorkflowClient{}
			controller := &Controller{workflows: client, config: config}
			hub := &createCache{testObservationHub: testObservationHub{controller: controller}}
			controller.WithWatchHub(hub)
			gets := 0
			client.getHook = func(context.Context, string) (*wfv1.Workflow, error) {
				gets++
				if mode == "live" {
					return intended, nil
				}
				if mode == "unreachable" {
					return nil, errors.New("API unreachable")
				}
				return nil, apierrors.NewNotFound(workflowTestResource, request.Name)
			}
			client.postHook = func(context.Context, *wfv1.Workflow) (*wfv1.Workflow, error) {
				if mode == "hub" || mode == "foreign" {
					hub.workflows = []*wfv1.Workflow{intended.DeepCopy()}
				}
				if mode == "foreign" {
					hub.workflows[0].Annotations[runIDAnnotation] = "other"
				}
				return nil, apierrors.NewServiceUnavailable("lost acknowledgement")
			}
			_, _, err = controller.createWorkflow(t.Context(), request, intended)
			wantError := mode == "missing" || mode == "unreachable" || mode == "foreign"
			if (err != nil) != wantError {
				t.Fatal(err)
			}
			wantGets := 1
			if mode == "hub" || mode == "foreign" {
				wantGets = 0
			}
			if gets != wantGets || len(client.posts) != 1 {
				t.Fatalf("GET=%d POST=%d", gets, len(client.posts))
			}
		})
	}
}

type lateCreateCache struct {
	testObservationHub
	mu           sync.Mutex
	workflows    []*wfv1.Workflow
	wake         chan struct{}
	subscribed   chan struct{}
	unsubscribed chan struct{}
}

func (h *lateCreateCache) WorkflowsForRun(string) []*wfv1.Workflow {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]*wfv1.Workflow(nil), h.workflows...)
}
func (h *lateCreateCache) Subscribe(string) (<-chan struct{}, func()) {
	close(h.subscribed)
	return h.wake, func() {
		if h.unsubscribed != nil {
			close(h.unsubscribed)
		}
	}
}

func TestDirectCreateLateUnwantedWorkflowGetsStopWithoutPolling(t *testing.T) {
	for _, trigger := range []string{"branch", "tag", "promotion", "release", "apply"} {
		for _, foreign := range []bool{false, true} {
			t.Run(trigger+fmt.Sprint(foreign), func(t *testing.T) {
				cfg := seedTestConfig()
				request := testRequest(pipeline.TriggerCI, greedyDocument)
				wf, err := Build(cfg, request)
				if err != nil {
					t.Fatal(err)
				}
				request.RunTrigger = argoworkflow.RunTrigger(trigger)
				wf.UID = "original"
				if foreign {
					wf.Annotations[runIDAnnotation] = "foreign"
				}
				client := &createRetryWorkflowClient{stored: wf}
				controller := &Controller{workflows: client, config: cfg}
				hub := &lateCreateCache{testObservationHub: testObservationHub{controller: controller}, wake: make(chan struct{}, 1), subscribed: make(chan struct{})}
				controller.WithWatchHub(hub)
				gets := 0
				client.getHook = func(context.Context, string) (*wfv1.Workflow, error) {
					gets++
					return nil, errors.New("unexpected live GET")
				}
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				done := make(chan error, 1)
				go func() { done <- controller.WatchFailedCreate(ctx, request) }()
				<-hub.subscribed
				hub.mu.Lock()
				hub.workflows = []*wfv1.Workflow{wf}
				hub.mu.Unlock()
				hub.wake <- struct{}{}
				err = <-done
				want := wfv1.ShutdownStrategyTerminate
				if trigger == "branch" {
					want = wfv1.ShutdownStrategyStop
				}
				if foreign {
					if err == nil || client.stored.Spec.Shutdown != "" {
						t.Fatalf("foreign Workflow changed: %v", err)
					}
				} else if err != nil || client.stored.Spec.Shutdown != want {
					t.Fatalf("late Workflow not stopped: %v", err)
				}
				if gets != 0 || len(client.posts) != 0 {
					t.Fatalf("GET=%d POST=%d", gets, len(client.posts))
				}
			})
		}
	}
}

func TestDirectCreateFailedWatchEndsAndUnsubscribesWithoutAnObject(t *testing.T) {
	controller := &Controller{}
	hub := &lateCreateCache{testObservationHub: testObservationHub{controller: controller}, wake: make(chan struct{}), subscribed: make(chan struct{}), unsubscribed: make(chan struct{})}
	controller.WithWatchHub(hub)
	ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond)
	defer cancel()
	if err := controller.WatchFailedCreate(ctx, testRequest(pipeline.TriggerCI, greedyDocument)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("watch did not end: %v", err)
	}
	select {
	case <-hub.unsubscribed:
	default:
		t.Fatal("expired failed-create watch retained its subscription")
	}
}
