package argojob

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/pipeline"
)

var workflowTestResource = schema.GroupResource{Group: "argoproj.io", Resource: "workflows"}

type createRetryWorkflowClient struct {
	posts    []*wfv1.Workflow
	stored   *wfv1.Workflow
	getHook  func(context.Context, string) (*wfv1.Workflow, error)
	postHook func(context.Context, *wfv1.Workflow) (*wfv1.Workflow, error)
}

func (c *createRetryWorkflowClient) Get(ctx context.Context, name string, _ metav1.GetOptions) (*wfv1.Workflow, error) {
	if c.getHook != nil {
		return c.getHook(ctx, name)
	}
	if c.stored == nil {
		return nil, apierrors.NewNotFound(workflowTestResource, name)
	}
	return c.stored.DeepCopy(), nil
}
func (c *createRetryWorkflowClient) Create(ctx context.Context, wf *wfv1.Workflow, _ metav1.CreateOptions) (*wfv1.Workflow, error) {
	c.posts = append(c.posts, wf.DeepCopy())
	if c.postHook != nil {
		return c.postHook(ctx, wf)
	}
	c.stored = wf.DeepCopy()
	c.stored.UID = "persisted-workflow"
	return c.stored.DeepCopy(), nil
}
func (*createRetryWorkflowClient) Delete(context.Context, string, metav1.DeleteOptions) error {
	return errors.New("unexpected Workflow deletion")
}

func TestWorkflowCreateOnceAdoptsOnlyItsRunOwner(t *testing.T) {
	for _, mode := range []string{"created", "already-exists", "committed", "api-down", "foreign-run", "foreign-owner", "native-default"} {
		t.Run(mode, func(t *testing.T) {
			cfg := seedTestConfig()
			request := testRequest(pipeline.TriggerCI, greedyDocument)
			request.SourceVolume = SourceVolume{ClaimName: "source"}
			intended, err := Build(cfg, request)
			if err != nil {
				t.Fatal(err)
			}
			client := &createRetryWorkflowClient{}
			client.postHook = func(_ context.Context, wf *wfv1.Workflow) (*wfv1.Workflow, error) {
				if mode == "created" {
					return wf, nil
				}
				if mode != "api-down" {
					client.stored = wf.DeepCopy()
					client.stored.UID = "original"
					switch mode {
					case "foreign-run":
						client.stored.Labels[runLabel] = "foreign"
					case "foreign-owner":
						client.stored.Labels["oberth.ci/repo"] = "foreign"
					case "native-default":
						client.stored.Spec.Parallelism = new(int64(5))
					}
				}
				if mode == "committed" || mode == "api-down" {
					return nil, apierrors.NewServiceUnavailable("cluster unavailable")
				}
				return nil, apierrors.NewAlreadyExists(workflowTestResource, wf.Name)
			}
			controller := &Controller{workflows: client, config: cfg}
			got, _, err := controller.createWorkflow(t.Context(), request, intended)
			fail := mode == "api-down" || strings.HasPrefix(mode, "foreign-")
			if (err != nil) != fail {
				t.Fatalf("got=%v err=%v", got, err)
			}
			if len(client.posts) != 1 {
				t.Fatalf("POST count=%d", len(client.posts))
			}
			if mode == "api-down" && !strings.Contains(err.Error(), "request another attempt explicitly") {
				t.Fatal(err)
			}
		})
	}
}

// Authored commands keep their admission semantics at every recovery boundary.
func TestAdmittedUsageCommandSurvivesIdempotentCreateAndAdoption(t *testing.T) {
	config := seedTestConfig()
	kube := fake.NewClientset()
	runningSeedPods(kube)
	client := &createRetryWorkflowClient{}
	client.postHook = func(_ context.Context, workflow *wfv1.Workflow) (*wfv1.Workflow, error) {
		client.stored = workflow.DeepCopy()
		client.stored.UID = "persisted-workflow"
		return nil, apierrors.NewServerTimeout(workflowTestResource, "post", 0)
	}
	controller, err := NewController(client, kube, config)
	if err != nil {
		t.Fatal(err)
	}
	var seeds []seedExecCall
	controller.WithSourceSeeder(NewSourceSeeder(kube, recordingExec(&seeds), config))
	source := strings.Replace(greedyDocument, "command: [/bin/true]", "command: [/run/oberth/usage/tool]", 1)
	request := testRequest(pipeline.TriggerCI, source)
	request.SourceDir = writeCheckout(t)
	// Actual Create admits and compiles the document, then reconciles a lost
	// POST acknowledgement. It must neither rewrite the command nor POST again.
	if name, err := controller.Create(t.Context(), request); err != nil || name != request.Name {
		t.Fatalf("ambiguous Create = %q, %v", name, err)
	}
	before, seedCount := client.stored.DeepCopy(), len(seeds)
	t.Run("create-retry", func(t *testing.T) {
		if name, err := controller.Create(t.Context(), request); err != nil || name != request.Name {
			t.Fatalf("retry of admitted command = %q, %v", name, err)
		}
	})
	t.Run("adoption", func(t *testing.T) {
		binding, found, err := controller.Adopt(t.Context(), AdoptionRequest{
			Name: request.Name, RunID: request.RunID, Ref: request.Ref, SHA: request.SHA,
			Trigger: request.Trigger, RunTrigger: argoworkflow.RunTriggerBranch,
		})
		if err != nil || !found || binding.UID != before.UID {
			t.Fatalf("adoption of admitted command = %v, %v, %v", binding.UID, found, err)
		}
	})
	if len(client.posts) != 1 || len(seeds) != seedCount || !reflect.DeepEqual(before, client.stored) {
		t.Fatal("recovery reseeded, recreated or mutated the submitted Workflow")
	}
	if got := client.stored.Spec.Templates[0].Container.Command; !reflect.DeepEqual(got, []string{"/run/oberth/usage/tool"}) {
		t.Fatalf("admitted command changed: %q", got)
	}
}
