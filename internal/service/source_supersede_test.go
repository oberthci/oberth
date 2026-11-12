package service

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/model"
)

type supersededPendingSourceJobs struct{ pendingSourceJobs }

func (j supersededPendingSourceJobs) CreateWorkflow(ctx context.Context, r WorkflowRequest) error {
	if r.Run.SHA != strings.Repeat("a", 40) {
		return j.groupEngine.CreateWorkflow(ctx, r)
	}
	return j.pendingSourceJobs.CreateWorkflow(ctx, r)
}

func TestSupersedeCleansPendingSourcePodAndVolume(t *testing.T) {
	f := newGroupFixture(t, 0)
	client := fake.NewClientset()
	ready := make(chan struct{})
	client.PrependReactor("create", "pods", func(a ktesting.Action) (bool, runtime.Object, error) {
		a.(ktesting.CreateAction).GetObject().(*corev1.Pod).Status.Phase = corev1.PodPending
		close(ready)
		return false, nil, nil
	})
	seeder := argojob.NewSourceSeeder(client, func(context.Context, string, string, string, []string, io.Reader, io.Writer, io.Writer) error {
		return errors.New("superseded Pending seed must not execute")
	}, argojob.Config{Namespace: "pipeline", SeedPollInterval: time.Millisecond})
	finished := make(chan struct{})
	f.scheduler.jobs = supersededPendingSourceJobs{pendingSourceJobs{f.engine, seeder, finished}}
	old := f.branch(t, f.exampleService, "feature/source-supersede", "a")
	f.start(t)
	select {
	case <-ready:
	case <-time.After(testStartTimeout(15 * time.Second)):
		t.Fatal("seed was not created")
	}
	newer := f.branch(t, f.exampleService, old.Ref, "b")
	ended := f.awaitTerminal(t, old.ID)
	if ended.Status != model.RunInterrupted || ended.SupersededBy != newer.ID || ended.Reason != "superseded by newer run" {
		t.Fatal("old preparation not superseded", ended)
	}
	f.awaitNoPendingCancellations(t)
	select {
	case <-finished:
	case <-time.After(testStartTimeout(15 * time.Second)):
		t.Fatal("superseded source preparation did not stop")
	}
	pods, err := client.CoreV1().Pods("pipeline").List(t.Context(), metav1.ListOptions{})
	if err != nil || len(pods.Items) != 0 {
		t.Fatal("superseded seed retained", pods, err)
	}
	claims, err := client.CoreV1().PersistentVolumeClaims("pipeline").List(t.Context(), metav1.ListOptions{})
	if err != nil || len(claims.Items) != 0 {
		t.Fatal("superseded volume retained", claims, err)
	}
	if f.engine.wasCreated(old.ID) {
		t.Fatal("superseded preparation submitted a Workflow")
	}
	if made := f.awaitCreated(t, 1); !made[newer.ID] {
		t.Fatal("successor did not progress", made)
	}
	f.engine.finish(t, newer.ID)
	f.awaitTerminal(t, newer.ID)
}
