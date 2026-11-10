package argojob

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	argologging "github.com/argoproj/argo-workflows/v4/util/logging"
	argotemplate "github.com/argoproj/argo-workflows/v4/util/template"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/pipeline"
)

// resolvedMain is the main container Argo creates for one attempt of
// template name: the conditional patch resolved with the retry-local values
// and strategically merged over the template container.
func resolvedMain(t *testing.T, wf *wfv1.Workflow, name, retries string) corev1.Container {
	t.Helper()
	tmpl := templateNamed(t, wf, name)
	encoded, err := json.Marshal(tmpl)
	if err != nil {
		t.Fatal(err)
	}
	params := map[string]any{"retries": retries, "lastRetry.exitCode": "1", "lastRetry.status": "Failed", "lastRetry.duration": "5", "lastRetry.message": ""}
	ctx := argologging.WithLogger(context.Background(), argologging.NewSlogLogger(argologging.Info, argologging.Text))
	replaced, err := argotemplate.Replace(ctx, string(encoded), params, true)
	if err != nil {
		t.Fatal(err)
	}
	var resolved wfv1.Template
	if err := json.Unmarshal([]byte(replaced), &resolved); err != nil {
		t.Fatal(err)
	}
	container := *tmpl.Container
	container.Name = mainContainerName
	pod, _ := json.Marshal(corev1.PodSpec{Containers: []corev1.Container{container, {Name: "wait"}}})
	merged, err := strategicpatch.StrategicMergePatch(pod, []byte(resolved.PodSpecPatch), corev1.PodSpec{})
	if err != nil {
		t.Fatal(err)
	}
	var spec corev1.PodSpec
	if err := json.Unmarshal(merged, &spec); err != nil {
		t.Fatal(err)
	}
	for _, c := range spec.Containers {
		if c.Name == mainContainerName {
			return c
		}
	}
	t.Fatal("no main container")
	return corev1.Container{}
}

// #1518: a held attempt's Go caches move to private scratch, so its main
// container gets assist.scratchBytes more ephemeral-storage request and
// limit than attempt 1; attempt 1 is unchanged, and -1 adds nothing.
func TestAssistHeldAttemptGetsScratchEphemeralStorage(t *testing.T) {
	ephemeral := func(list corev1.ResourceList) int64 {
		value := list[corev1.ResourceEphemeralStorage]
		return value.Value()
	}
	wf, err := Build(assistConfig(), assistRequest(pipeline.TriggerCI, assistDocument))
	if err != nil {
		t.Fatal(err)
	}
	first, held := resolvedMain(t, wf, "test", "0"), resolvedMain(t, wf, "test", "1")
	if got, want := ephemeral(held.Resources.Limits), ephemeral(first.Resources.Limits)+DefaultAssistScratchBytes; got != want {
		t.Fatalf("held ephemeral limit = %d, want attempt 1's %d + %d", got, ephemeral(first.Resources.Limits), DefaultAssistScratchBytes)
	}
	if got, want := ephemeral(held.Resources.Requests), ephemeral(first.Resources.Requests)+DefaultAssistScratchBytes; got != want {
		t.Fatalf("held ephemeral request = %d, want %d", got, want)
	}
	if !held.Resources.Limits.Memory().Equal(*first.Resources.Limits.Memory()) || !held.Resources.Requests.Cpu().Equal(*first.Resources.Requests.Cpu()) {
		t.Fatalf("held attempt changed CPU or memory: %+v vs %+v", held.Resources, first.Resources)
	}
	config := assistConfig()
	config.Assist.ScratchBytes = -1
	none, err := Build(config, assistRequest(pipeline.TriggerCI, assistDocument))
	if err != nil {
		t.Fatal(err)
	}
	if got := resolvedMain(t, none, "test", "1"); ephemeral(got.Resources.Limits) != ephemeral(first.Resources.Limits) {
		t.Fatalf("scratchBytes -1 still widened the limit: %+v", got.Resources)
	}
	// A main without its own ephemeral-storage values gets the allowance
	// as both, so the Pod's total limit still grows by it.
	resources, err := assistScratchResources(nil, &corev1.Container{Name: mainContainerName}, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	if encoded, _ := json.Marshal(resources); !strings.Contains(string(encoded), `"limits":{"ephemeral-storage":"1Gi"}`) || !strings.Contains(string(encoded), `"requests":{"ephemeral-storage":"1Gi"}`) {
		t.Fatalf("resources without a base = %s", encoded)
	}
	for _, bad := range []int64{-2, MaxAssistScratchBytes + 1} {
		if err := (AssistConfig{ScratchBytes: bad}).Validate(); err == nil {
			t.Fatalf("scratch bytes %d accepted", bad)
		}
	}
}

// #1518: the kubelet evicting a held or continued attempt is recorded once
// as a server event with the kubelet's message, so the reason line names the
// eviction instead of a plain step failure.
func TestEnforceAssistHoldsRecordsEvictedAttemptOnce(t *testing.T) {
	now := time.Now()
	evicted := heldPodFixture("wf-a-test-1", "wf-a", "test-service", 30*time.Minute, false)
	started := evicted.Status.ContainerStatuses[0].State.Running.StartedAt
	evicted.Status.Phase, evicted.Status.Reason = corev1.PodFailed, "Evicted"
	evicted.Status.Message = "Pod ephemeral local storage usage exceeds the total limit of containers 1216Mi."
	evicted.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, StartedAt: started}}
	attempt := failedAttemptFixture(evicted, 1)
	ordinary := heldPodFixture("wf-b-test-0", "wf-b", "lint", time.Minute, false)
	ordinary.Annotations[argoNodeNameAnnotation] = "wf-b.lint(0)"
	ordinary.Status.Phase, ordinary.Status.Reason = corev1.PodFailed, "Evicted"
	client := fake.NewClientset(evicted, attempt, ordinary)
	abandoned, err := assistSweepController(t, client).EnforceAssistHolds(context.Background(), nil, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(abandoned) != 1 {
		t.Fatalf("abandoned = %+v, want the evicted held attempt only", abandoned)
	}
	got := abandoned[0]
	if got.Pod != "wf-a-test-1" || got.Step != "test-service" || got.Reason != model.AssistAbandonEvicted || got.Eviction != evicted.Status.Message ||
		!got.HeldSince.Equal(started.Time) || got.FailedExitCode == nil || *got.FailedExitCode != 1 {
		t.Fatalf("eviction = %+v", got)
	}
	if _, err := client.CoreV1().Pods(testNamespace).Get(context.Background(), "wf-a-test-1", metav1.GetOptions{}); err != nil {
		t.Fatal("the sweep deleted the evicted Pod; it only records it")
	}
	recorded := map[string]model.AssistPodActivity{"wf-a-test-1": {Released: true, Evicted: true}}
	if again, err := assistSweepController(t, client).EnforceAssistHolds(context.Background(), recorded, nil, now); err != nil || len(again) != 0 {
		t.Fatalf("a recorded eviction was reported again: %+v %v", again, err)
	}
	finished := func(context.Context, string, string) (model.AssistPodActivity, error) {
		return model.AssistPodActivity{Released: true, Evicted: true}, nil
	}
	if again, err := assistSweepController(t, client).EnforceAssistHolds(context.Background(), nil, finished, now); err != nil || len(again) != 0 {
		t.Fatalf("an eviction recorded on a finished run was reported again: %+v %v", again, err)
	}
}
