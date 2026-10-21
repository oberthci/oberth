package argojob

import (
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runprogress"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

// scriptedWorkflowClient serves successive Workflow observations, repeating the
// last one, and calls exhausted once every scripted observation was read.
type scriptedWorkflowClient struct {
	staticWorkflowClient
	mu        sync.Mutex
	states    []*wfv1.Workflow
	reads     int
	exhausted func()
}

func (client *scriptedWorkflowClient) Get(context.Context, string, metav1.GetOptions) (*wfv1.Workflow, error) {
	client.mu.Lock()
	defer client.mu.Unlock()
	index := min(client.reads, len(client.states)-1)
	client.reads++
	if client.reads > len(client.states) && client.exhausted != nil {
		client.exhausted()
	}
	return client.states[index].DeepCopy(), nil
}

// failingTimingRecorder surfaces store refusals that the controller otherwise
// only logs, so a re-attributed attempt cannot pass unnoticed.
type failingTimingRecorder struct {
	*store.Store
	errs []error
}

func (r *failingTimingRecorder) RecordWorkflowTiming(ctx context.Context, value model.WorkflowTiming) error {
	err := r.Store.RecordWorkflowTiming(ctx, value)
	if err != nil {
		r.errs = append(r.errs, err)
	}
	return err
}

func (r *failingTimingRecorder) RecordStepTimings(ctx context.Context, values []model.StepTiming) error {
	err := r.Store.RecordStepTimings(ctx, values)
	if err != nil {
		r.errs = append(r.errs, err)
	}
	return err
}

// An adopted branch Workflow (#1172) resumes observation under the original
// run and Workflow UID. Its timing must bind to the same durable attempt
// records the previous owner wrote, without duplicate or re-attributed rows.
func TestAdoptedWaitRetainsTimingWithoutDuplicateAttempts(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "timing.db")
	db, err := store.Open(ctx, path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	upstream, err := db.CreateUpstream(ctx, model.UpstreamSpec{Name: "codeberg", Kind: "forgejo", BaseURL: "https://codeberg.org"})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := db.CreateRepository(ctx, model.RepositorySpec{Name: "oberth", UpstreamID: upstream.ID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("b", 40)
	run, err := db.EnqueueRun(ctx, model.RunSpec{RepoID: repo.ID, RefKind: model.RefBranch, Ref: "feature/adopt", SHA: sha, Actor: "SHA256:test", Trigger: "push"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}

	terminal, first, pod, at := diagnosticFixture()
	terminal.CreationTimestamp = metav1.NewTime(at.Add(-10 * time.Minute))
	terminal.Annotations[runIDAnnotation] = run.ID
	terminal.Annotations[identityAnnotation] = strings.Repeat("a", 64)
	terminal.Annotations[refAnnotation] = "feature/adopt"
	terminal.Annotations[argoworkflow.SizeAnnotation] = "L"
	terminal.Annotations[argoworkflow.ConcurrencyGroupAnnotation] = "build"
	terminal.Labels = map[string]string{"oberth.ci/sha": sha, "oberth.ci/trigger": string(periapsis.TriggerCI)}
	terminal.Spec.Entrypoint = "dag"
	terminal.Spec.Templates = []wfv1.Template{
		{Name: "dag", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "test", Template: "leaf"}, {Name: "unit", Template: "leaf"}}}},
		{Name: "leaf", Container: &corev1.Container{Name: "main"}},
	}
	if _, err = db.SetRunJobName(ctx, run.ID, terminal.Name); err != nil {
		t.Fatal(err)
	}
	first.Phase = wfv1.NodeSucceeded
	first.Outputs = &wfv1.Outputs{ExitCode: ptr.To("0")}
	firstPod := pod.DeepCopy()
	firstPod.Status.ContainerStatuses[0].State.Terminated.ExitCode = 0
	firstPod.Status.ContainerStatuses[0].State.Terminated.Reason = "Completed"
	second := *first.DeepCopy()
	second.ID = "workflow-test-5678"
	second.Name = "workflow-test.unit"
	second.DisplayName = "unit"
	second.StartedAt = metav1.NewTime(at)
	second.FinishedAt = metav1.NewTime(at.Add(30 * time.Second))
	secondPod := firstPod.DeepCopy()
	secondPod.Name = PodName(terminal, second)
	secondPod.UID = "33333333-3333-4333-8333-333333333333"
	secondPod.Annotations = map[string]string{"workflows.argoproj.io/node-id": second.ID, "workflows.argoproj.io/node-name": second.Name}
	cid := "containerd://" + strings.Repeat("e", 64)
	secondPod.Status.ContainerStatuses[0].ContainerID = cid
	secondPod.Status.ContainerStatuses[0].State.Terminated.ContainerID = cid
	secondPod.Status.ContainerStatuses[0].State.Terminated.StartedAt = second.StartedAt
	secondPod.Status.ContainerStatuses[0].State.Terminated.FinishedAt = second.FinishedAt
	terminal.Status.Phase = wfv1.WorkflowSucceeded
	terminal.Status.Nodes = wfv1.Nodes{first.ID: first, second.ID: second}

	// The previous owner observed the first leaf finish while the second ran.
	running := terminal.DeepCopy()
	running.Status.Phase = wfv1.WorkflowRunning
	runningSecond := second
	runningSecond.Phase = wfv1.NodeRunning
	runningSecond.FinishedAt = metav1.Time{}
	runningSecond.Outputs = nil
	running.Status.Nodes[second.ID] = runningSecond
	logs := func(context.Context, string, string, string) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("log line\n")), nil
	}
	config := Config{Namespace: terminal.Namespace, MaxRunLogBytes: 1 << 20}

	recorder := &failingTimingRecorder{Store: db}
	ownerCtx, stop := context.WithCancel(ctx)
	defer stop()
	owner := &Controller{workflows: &scriptedWorkflowClient{states: []*wfv1.Workflow{running}, exhausted: stop}, kube: fake.NewClientset(firstPod.DeepCopy()), logs: logs, config: config, interval: time.Millisecond, timings: recorder}
	if _, err = owner.Wait(ownerCtx, terminal.Name, run.ID, io.Discard); err == nil {
		t.Fatal("pre-restart owner observed a terminal Workflow")
	}
	before, err := db.RunTiming(ctx, run.ID)
	if err != nil || before.Workflow == nil || len(before.Steps) != 1 || !before.Steps[0].Available || before.Steps[0].NodeID != first.ID || len(recorder.errs) != 0 {
		t.Fatalf("pre-restart timing = %+v, %v, errs=%v", before, err, recorder.errs)
	}
	retainedFirst, _ := json.Marshal(before.Steps[0])
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}

	// Restart: the first leaf's Pod was garbage collected while no owner ran.
	db, err = store.Open(ctx, path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	recorder = &failingTimingRecorder{Store: db}
	request := AdoptionRequest{Name: terminal.Name, RunID: run.ID, Ref: "feature/adopt", SHA: sha, Group: "build", Trigger: periapsis.TriggerCI, RunTrigger: argoworkflow.RunTriggerBranch}
	binding, found, err := (&Controller{workflows: &staticWorkflowClient{workflow: running}, config: config}).Adopt(ctx, request)
	if err != nil || !found {
		t.Fatalf("adopt: %v %v", found, err)
	}
	steps := StepResults(running)
	var events []runprogress.Event
	for _, step := range steps {
		if step.NodeID == first.ID {
			events = append(events, runprogress.Event{Version: runprogress.Version, Burn: step.Burn, Step: step.Step, Ordinal: step.Ordinal, Status: step.Status, StartedAt: step.StartedAt, FinishedAt: step.FinishedAt})
		}
	}
	resumed := &Controller{workflows: &scriptedWorkflowClient{states: []*wfv1.Workflow{running, terminal}}, kube: fake.NewClientset(secondPod.DeepCopy()), logs: logs, config: config, interval: time.Millisecond, timings: recorder}
	completion, err := resumed.WaitResumed(ctx, terminal.Name, run.ID, io.Discard, events, 100, binding)
	if err != nil || !completion.Succeeded || len(completion.Steps) != 2 {
		t.Fatalf("resumed completion = %+v, %v", completion, err)
	}
	if len(recorder.errs) != 0 {
		t.Fatalf("adopted observation re-attributed an attempt: %v", recorder.errs)
	}
	after, err := db.RunTiming(ctx, run.ID)
	if err != nil || after.Workflow == nil || len(after.Steps) != 2 {
		t.Fatalf("resumed timing = %+v, %v", after, err)
	}
	a, _ := json.Marshal(before.Workflow)
	b, _ := json.Marshal(after.Workflow)
	if string(a) != string(b) || after.Workflow.UID != string(terminal.UID) {
		t.Fatalf("Workflow binding changed across adoption: %s / %s", a, b)
	}
	seen := map[string]bool{}
	for _, step := range after.Steps {
		if seen[step.NodeID] || step.WorkflowUID != string(terminal.UID) || !step.Available {
			t.Fatalf("duplicate or unbound attempt: %+v", after.Steps)
		}
		seen[step.NodeID] = true
		raw, _ := json.Marshal(step)
		if step.NodeID == first.ID && string(raw) != string(retainedFirst) {
			t.Fatalf("pre-restart evidence changed by adopted observation: %s / %s", retainedFirst, raw)
		}
		if step.NodeID == second.ID && (step.PodUID != string(secondPod.UID) || step.Main == nil || step.Main.ContainerID != cid) {
			t.Fatalf("adopted leaf timing = %+v", step)
		}
	}
	if !seen[first.ID] || !seen[second.ID] {
		t.Fatalf("missing node timing: %+v", after.Steps)
	}
}
