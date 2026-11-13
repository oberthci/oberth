package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/model"
)

type pendingSourceJobs struct {
	*groupEngine
	seeder   *argojob.SourceSeeder
	finished chan struct{}
}

func (j pendingSourceJobs) CreateWorkflow(ctx context.Context, r WorkflowRequest) error {
	defer close(j.finished)
	_, err := j.seeder.SeedRequest(ctx, argojob.Request{Name: r.WorkflowName, SourceDir: r.SourceDir}, false)
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

func TestPendingSourcePodLossFailsTheRunAndCleansItsVolume(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(fmt.Sprint(terminal), func(t *testing.T) {
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
			run := f.branch(t, f.exampleService, "feature/source-loss", "a")
			f.start(t)
			select {
			case <-ready:
			case <-time.After(testStartTimeout(15 * time.Second)):
				t.Fatal("seed was not created")
			}
			var pod corev1.Pod
			deadline := time.Now().Add(testStartTimeout(5 * time.Second))
			for {
				pods, err := client.CoreV1().Pods("pipeline").List(t.Context(), metav1.ListOptions{})
				if err != nil {
					t.Fatal(err)
				}
				if len(pods.Items) == 1 {
					pod = pods.Items[0]
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("created seed not visible")
				}
				time.Sleep(time.Millisecond)
			}
			if terminal {
				pod.Status.Phase = corev1.PodFailed
				if _, err := client.CoreV1().Pods("pipeline").UpdateStatus(t.Context(), &pod, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
			} else if err := client.CoreV1().Pods("pipeline").Delete(t.Context(), pod.Name, metav1.DeleteOptions{}); err != nil {
				t.Fatal(err)
			}
			ended := f.awaitTerminal(t, run.ID)
			want := "disappeared before source preparation completed"
			if terminal {
				want = "ended in phase Failed"
			}
			if ended.Status != model.RunFailed || ended.Phase != "infrastructure" || !strings.Contains(ended.Error, want) || !strings.HasPrefix(presentationRun(ended).DisplayReason, "source seed Pod") {
				t.Fatal("seed loss not reported on this run", ended)
			}
			select {
			case <-finished:
			case <-time.After(testStartTimeout(15 * time.Second)):
				t.Fatal("source preparation remained running")
			}
			claims, err := client.CoreV1().PersistentVolumeClaims("pipeline").List(t.Context(), metav1.ListOptions{})
			if err != nil || len(claims.Items) != 0 {
				t.Fatal("source volume retained", claims, err)
			}
			if f.engine.wasCreated(run.ID) {
				t.Fatal("lost seed submitted a Workflow")
			}
		})
	}
}
