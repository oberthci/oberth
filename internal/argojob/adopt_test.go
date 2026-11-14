package argojob

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/pipeline"
)

func adoptionWorkflow() (*wfv1.Workflow, AdoptionRequest) {
	wf := budgetTestWorkflow()
	wf.UID = "workflow-uid"
	wf.Annotations[helperStorageAnnotation] = helperStoragePolicy
	wf.Annotations[identityAnnotation] = strings.Repeat("a", 64)
	wf.Annotations[refAnnotation] = "feature/adopt"
	wf.Labels = map[string]string{"oberth.ci/sha": strings.Repeat("b", 40), "oberth.ci/trigger": string(pipeline.TriggerCI)}
	return wf, AdoptionRequest{Name: wf.Name, RunID: wf.Annotations[runIDAnnotation], Ref: "feature/adopt", SHA: strings.Repeat("b", 40), Trigger: pipeline.TriggerCI, RunTrigger: argoworkflow.RunTriggerBranch}
}

func TestAdoptionBindsDurableRunAndSubmittedResources(t *testing.T) {
	wf, request := adoptionWorkflow()
	for _, test := range []struct {
		name   string
		mutate func(*wfv1.Workflow)
	}{
		{"valid", func(*wfv1.Workflow) {}},
		{"foreign-run", func(wf *wfv1.Workflow) { wf.Annotations[runIDAnnotation] = "other" }},
		{"missing-spec-identity", func(wf *wfv1.Workflow) { delete(wf.Annotations, identityAnnotation) }},
		{"missing-uid", func(wf *wfv1.Workflow) { wf.UID = "" }},
		{"foreign-ref", func(wf *wfv1.Workflow) { wf.Annotations[refAnnotation] = "main" }},
		{"foreign-sha", func(wf *wfv1.Workflow) { wf.Labels["oberth.ci/sha"] = "other" }},
		{"foreign-trigger", func(wf *wfv1.Workflow) { wf.Labels["oberth.ci/trigger"] = "release" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := wf.DeepCopy()
			test.mutate(candidate)
			controller := &Controller{workflows: &staticWorkflowClient{workflow: candidate}, config: Config{Namespace: wf.Namespace}}
			binding, found, err := controller.Adopt(context.Background(), request)
			if test.name == "valid" {
				if err != nil || !found || binding.UID != wf.UID || binding.Identity != wf.Annotations[identityAnnotation] {
					t.Fatalf("adoption = %#v, %v, %v", binding, found, err)
				}
			} else if err == nil || !errors.Is(err, ErrAdoptionRefused) {
				t.Fatalf("invalid adoption not refused: %#v, %v, %v", binding, found, err)
			}
		})
	}
	controller := &Controller{workflows: missingWorkflowClient{}, config: Config{Namespace: wf.Namespace}}
	if _, found, err := controller.Adopt(context.Background(), request); err != nil || found {
		t.Fatalf("missing Workflow = %v, %v", found, err)
	}
	controller = &Controller{workflows: &flakyWorkflowClient{staticWorkflowClient: staticWorkflowClient{workflow: wf}, failures: 1}, config: Config{Namespace: wf.Namespace}}
	if _, found, err := controller.Adopt(context.Background(), request); err == nil || found || errors.Is(err, ErrAdoptionRefused) {
		t.Fatalf("read failure = %v, %v; want an unrefused error", found, err)
	}
}

// flakyWorkflowClient fails the first reads the way an API server restart or
// connection reset does, then serves the Workflow.
type flakyWorkflowClient struct {
	staticWorkflowClient
	mu       sync.Mutex
	failures int
	reads    int
}

func (client *flakyWorkflowClient) Get(ctx context.Context, name string, options metav1.GetOptions) (*wfv1.Workflow, error) {
	client.mu.Lock()
	client.reads++
	failing := client.failures < 0 || client.reads <= client.failures
	client.mu.Unlock()
	if failing {
		return nil, errors.New("connection reset by peer")
	}
	return client.staticWorkflowClient.Get(ctx, name, options)
}

func TestAdoptedWaitRidesOutTransientReadFailures(t *testing.T) {
	wf, request := adoptionWorkflow()
	binding, found, err := (&Controller{workflows: &staticWorkflowClient{workflow: wf}, config: Config{Namespace: wf.Namespace}}).Adopt(context.Background(), request)
	if err != nil || !found {
		t.Fatalf("adopt: %v %v", found, err)
	}
	client := &flakyWorkflowClient{staticWorkflowClient: staticWorkflowClient{workflow: wf}, failures: 3}
	controller := &Controller{workflows: client, config: Config{Namespace: wf.Namespace, MaxRunLogBytes: 4096}, idleInterval: time.Millisecond}
	completion, err := withWaitTestHub(t, controller).WaitResumed(context.Background(), wf.Name, request.RunID, io.Discard, binding)
	if err != nil || !completion.Succeeded || client.reads <= 3 {
		t.Fatalf("transient read failure ended adopted Wait: %#v %v reads=%d", completion, err, client.reads)
	}

	client = &flakyWorkflowClient{staticWorkflowClient: staticWorkflowClient{workflow: wf}, failures: -1}
	controller = &Controller{workflows: client, config: Config{Namespace: wf.Namespace, MaxRunLogBytes: 4096}, idleInterval: time.Millisecond}
	timeout, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	completion, err = withWaitTestHub(t, controller).WaitResumed(timeout, wf.Name, request.RunID, io.Discard, binding)
	if !errors.Is(err, context.DeadlineExceeded) || completion.Name != "" || client.reads < 2 {
		t.Fatalf("persistent read failure = %#v %v reads=%d", completion, err, client.reads)
	}
}

func TestAdoptedWaitRestoresProgressWithoutReadingPodLogs(t *testing.T) {
	wf, request := adoptionWorkflow()
	calls := map[string]int{}
	controller := &Controller{kube: fixtureKube(wf), workflows: &staticWorkflowClient{workflow: wf}, config: Config{Namespace: wf.Namespace, MaxRunLogBytes: 4096}, idleInterval: time.Millisecond,
		containerLogSource: func(_ context.Context, _, pod string, _ corev1.PodLogOptions) (io.ReadCloser, error) {
			calls[pod]++
			return io.NopCloser(strings.NewReader("new log line\n")), nil
		}}
	binding, found, err := controller.Adopt(context.Background(), request)
	if err != nil || !found {
		t.Fatalf("adopt: %v %v", found, err)
	}
	steps := fixtureStepResults(wf)
	var output bytes.Buffer
	completion, err := withWaitTestHub(t, controller).WaitResumed(context.Background(), wf.Name, request.RunID, &output, binding)
	if err != nil || !completion.Succeeded || len(completion.Steps) != 2 {
		t.Fatalf("resumed completion = %#v, %v", completion, err)
	}
	if calls[steps[0].PodName] != 0 || calls[steps[1].PodName] != 0 || strings.Contains(output.String(), "compile") || strings.Contains(output.String(), "new log line") {
		t.Fatalf("duplicate replay: calls=%v output=%s", calls, output.String())
	}
	output.Reset()
	_, err = withWaitTestHub(t, controller).WaitResumed(context.Background(), wf.Name, request.RunID, &output, binding)
	if err != nil || output.Len() != 0 || calls[steps[1].PodName] != 0 {
		t.Fatalf("second resume replayed completed steps: %v %s %v", err, output.String(), calls)
	}

}

func TestAdoptedWaitRefusesReplacedWorkflow(t *testing.T) {
	for _, mutation := range []string{"uid", "identity", "spec"} {
		wf, request := adoptionWorkflow()
		controller := &Controller{workflows: &staticWorkflowClient{workflow: wf}, config: Config{Namespace: wf.Namespace, MaxRunLogBytes: 4096}, idleInterval: time.Millisecond}
		binding, _, err := controller.Adopt(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		switch mutation {
		case "uid":
			wf.UID = "replacement"
		case "identity":
			wf.Annotations[identityAnnotation] = strings.Repeat("c", 64)
		case "spec":
			wf.Spec.Templates[3].Container.Image = "foreign-image"
		}
		if _, err := withWaitTestHub(t, controller).WaitResumed(context.Background(), wf.Name, request.RunID, io.Discard, binding); err == nil {
			t.Fatalf("Workflow replacement accepted: %s", mutation)
		}
	}
}

func TestAdoptionAcceptsActualServerBuiltWorkflow(t *testing.T) {
	request := testRequest(pipeline.TriggerCI, greedyDocument)
	workflow, err := Build(testConfig(), request)
	if err != nil {
		t.Fatal(err)
	}
	workflow.UID = "server-assigned-uid"
	controller := &Controller{workflows: &staticWorkflowClient{workflow: workflow}, config: testConfig()}
	binding, found, err := controller.Adopt(context.Background(), AdoptionRequest{Name: request.Name, RunID: request.RunID, Ref: request.Ref, SHA: request.SHA, Trigger: request.Trigger, RunTrigger: argoworkflow.RunTriggerBranch})
	if err != nil || !found {
		t.Fatalf("built Workflow adoption = %#v %v %v", binding, found, err)
	}
}

func TestAdoptionRefusesLegacyVMIWorkflowImmediately(t *testing.T) {
	wf, request := adoptionWorkflow()
	wf.Spec.Templates = append(wf.Spec.Templates, wfv1.Template{Name: "vm", Resource: &wfv1.ResourceTemplate{Action: "create"}})
	controller := &Controller{workflows: &staticWorkflowClient{workflow: wf}, config: Config{Namespace: wf.Namespace}}
	if _, found, err := controller.Adopt(t.Context(), request); found || !errors.Is(err, ErrAdoptionRefused) || !strings.Contains(err.Error(), "workflow capability grant") {
		t.Fatalf("legacy workflow not refused early: found=%v error=%v", found, err)
	}
}

type missingWorkflowClient struct{ WorkflowClient }

func (missingWorkflowClient) Get(_ context.Context, name string, _ metav1.GetOptions) (*wfv1.Workflow, error) {
	return nil, apierrors.NewNotFound(schema.GroupResource{Group: "argoproj.io", Resource: "workflows"}, name)
}

// v0.21.10 policy metadata is historical execution evidence, never a policy input for the new owner.
func TestAdoptionIgnoresRetiredSchedulingAnnotations(t *testing.T) {
	wf, request := adoptionWorkflow()
	for _, key := range []string{"oberth.ci/cpu-requests", "oberth.ci/request-defaults", "oberth.ci/declared-pod-demand", "oberth.ci/resource-admission-fallback"} {
		wf.Annotations[key] = "historical"
	}
	wf.Spec.PodPriorityClassName = "oberth-pipeline"
	wf.Spec.Templates[0].Metadata.Annotations = map[string]string{"oberth.ci/step-deadline-seconds": "60", "oberth.ci/step-start-allowance-seconds": "120"}
	before := wf.DeepCopy()
	controller := &Controller{workflows: &staticWorkflowClient{workflow: wf}, config: Config{Namespace: wf.Namespace}}
	binding, found, err := controller.Adopt(t.Context(), request)
	if err != nil || !found || !binding.matches(wf) {
		t.Fatal(binding, found, err)
	}
	if !reflect.DeepEqual(before, wf) {
		t.Fatal("adoption rewrote a running Workflow")
	}
}
