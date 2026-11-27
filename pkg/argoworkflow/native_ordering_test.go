package argoworkflow

import (
	"os"
	"slices"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	argosync "github.com/argoproj/argo-workflows/v4/workflow/sync"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestNativeWorkflowOrderAndBoundedSparseRepoProgress(t *testing.T) {
	now := time.Unix(100, 0)
	var admitted []string
	queue := argosync.NewMultiThrottler(1, 0, func(key string) { admitted = append(admitted, key) })
	err := queue.Init([]wfv1.Workflow{
		{ObjectMeta: metav1.ObjectMeta{Name: "long-lint", Namespace: "runs"}, Status: wfv1.WorkflowStatus{Phase: wfv1.WorkflowRunning}},
		{ObjectMeta: metav1.ObjectMeta{Name: "awaiting-promotion", Namespace: "runs"}, Status: wfv1.WorkflowStatus{Phase: wfv1.WorkflowSucceeded}},
	})
	if err != nil {
		t.Fatal(err)
	}
	queue.Add("runs/busy-lint", 0, now)
	queue.Add("runs/sparse-qualification", 10, now.Add(time.Second))
	queue.Add("runs/release", 30, now.Add(2*time.Second))
	if !queue.Admit("runs/long-lint") || queue.Admit("runs/release") || len(admitted) != 0 {
		t.Fatal("priority displaced running work", admitted)
	}
	queue.Remove("runs/long-lint")
	if !slices.Equal(admitted, []string{"runs/release"}) {
		t.Fatal("release did not take next slot", admitted)
	}
	queue.Remove("runs/release")
	if !slices.Equal(admitted, []string{"runs/release", "runs/sparse-qualification"}) {
		t.Fatal("sparse repo exceeded two turnovers in bounded trace", admitted)
	}
	t.Log("bounded trace: sparse qualification starts at turnover 2; no per-repo fair-share claim")
}

func TestNativeEqualClassOrderAndLowClassStarvation(t *testing.T) {
	now := time.Unix(100, 0)
	var admitted []string
	queue := argosync.NewMultiThrottler(1, 0, func(key string) { admitted = append(admitted, key) })
	queue.Add("runs/running", 0, now)
	queue.Add("runs/busy-z", 10, now.Add(time.Second))
	queue.Add("runs/sparse-a", 10, now.Add(time.Second))
	queue.Add("runs/older", 10, now)
	for _, completed := range []string{"runs/running", "runs/older", "runs/busy-z"} {
		queue.Remove(completed)
	}
	if !slices.Equal(admitted, []string{"runs/running", "runs/older", "runs/busy-z", "runs/sparse-a"}) {
		t.Fatal("native creation-time/key order differs", admitted)
	}
	t.Log("equal-class bounded trace: sparse repo starts at turnover 3; the two-turnover fairness target is not guaranteed")
	queue.Add("runs/low", 0, now)
	for _, high := range []string{"runs/high-a", "runs/high-b", "runs/high-c"} {
		queue.Add(high, 30, now)
	}
	for _, completed := range []string{"runs/sparse-a", "runs/high-a", "runs/high-b"} {
		queue.Remove(completed)
		if queue.Admit("runs/low") {
			t.Fatal("low priority unexpectedly bypassed higher backlog")
		}
	}
	t.Log("low class remains waiting for three turnovers under higher-priority backlog; starvation is possible")
}

func TestPinnedControllerPreservesPriorityAndSerialFanout(t *testing.T) {
	command := controllerFixture.Command(controllerContext(t), t, "-test.count=1", "-test.timeout=180s", "-test.run=^TestOberthNativePriorityAndSerialPods$", "-test.v")
	command.Env = append(os.Environ(), "TMPDIR="+os.Getenv("GOTMPDIR"))
	if raw, err := controllerOutput(command); err != nil {
		t.Fatalf("native ordering: %v\n%s", err, raw)
	}
}
