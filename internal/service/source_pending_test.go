package service

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/oberthci/oberth/internal/argojob"
)

type pendingSourceJobs struct {
	*groupEngine
	seeder   *argojob.SourceSeeder
	finished chan struct{}
}

func (j pendingSourceJobs) CreateWorkflow(ctx context.Context, r WorkflowRequest) error {
	defer close(j.finished)
	_, err := j.seeder.SeedRequest(ctx, argojob.Request{Name: r.WorkflowName, SourceDir: r.SourceDir}, false, false)
	if err != nil {
		return err
	}
	return j.groupEngine.CreateWorkflow(ctx, r)
}

func TestCancelAndRefDeletionCleanPendingSourcePodAndVolume(t *testing.T) {
	for _, operation := range []string{"cancel", "ref-delete"} {
		t.Run(operation, func(t *testing.T) {
			f := newGroupFixture(t, 0)
			client := fake.NewClientset()
			ready := make(chan struct{})
			client.PrependReactor("create", "pods", func(a ktesting.Action) (bool, k8sruntime.Object, error) {
				a.(ktesting.CreateAction).GetObject().(*corev1.Pod).Status.Phase = corev1.PodPending
				close(ready)
				return false, nil, nil
			})
			seeder := argojob.NewSourceSeeder(client, func(context.Context, string, string, string, []string, io.Reader, io.Writer, io.Writer) error {
				return errors.New("Pending seed must not execute")
			}, argojob.Config{Namespace: "pipeline", SeedPollInterval: time.Millisecond})
			finished := make(chan struct{})
			f.scheduler.jobs = pendingSourceJobs{f.engine, seeder, finished}
			run := f.branch(t, f.exampleService, "feature/pending-source", "a")
			f.start(t)
			select {
			case <-ready:
			case <-time.After(testStartTimeout(15 * time.Second)):
				t.Fatal("seed was not created")
			}
			if operation == "cancel" {
				mustRunCancel(t, f.control, cancelAdmin, map[string]any{"id": run.ID, "reason": "stop pending source"})
			} else {
				deleteFixtureBranch(t, f, f.exampleService, run.Ref)
			}
			f.awaitTerminal(t, run.ID)
			f.awaitNoPendingCancellations(t)
			select {
			case <-finished:
			case <-time.After(testStartTimeout(15 * time.Second)):
				t.Fatal("cancelled source preparation did not stop")
			}
			pods, err := client.CoreV1().Pods("pipeline").List(t.Context(), metav1.ListOptions{})
			if err != nil || len(pods.Items) != 0 {
				t.Fatalf("seed Pod retained: %+v %v", pods, err)
			}
			claims, err := client.CoreV1().PersistentVolumeClaims("pipeline").List(t.Context(), metav1.ListOptions{})
			if err != nil || len(claims.Items) != 0 {
				t.Fatalf("source volume retained: %+v %v", claims, err)
			}
			if f.engine.wasCreated(run.ID) {
				t.Fatal("cancelled preparation submitted a Workflow")
			}
		})
	}
}
