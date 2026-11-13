package argojob

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/oberthci/oberth/internal/model"
)

func TestPendingSourceSchedulingUsesExactSeedWithoutWorkflow(t *testing.T) {
	for _, kind := range []string{"image", "claim", "capacity"} {
		t.Run(kind, func(t *testing.T) {
			config := seedTestConfig()
			config.SeedPollInterval = time.Millisecond
			client := fake.NewClientset()
			ready := make(chan struct{})
			client.PrependReactor("create", "pods", func(a ktesting.Action) (bool, runtime.Object, error) {
				pod := a.(ktesting.CreateAction).GetObject().(*corev1.Pod)
				pod.UID = types.UID("seed-uid")
				pod.Status.Phase = corev1.PodPending
				switch kind {
				case "image":
					pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "seed", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "PRIVATE credentials"}}}}
				case "claim":
					pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable", Message: "pod has unbound immediate PersistentVolumeClaims PRIVATE"}}
				case "capacity":
					pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable", Message: "Insufficient cpu PRIVATE"}}
				}
				close(ready)
				return false, nil, nil
			})
			seeder := NewSourceSeeder(client, func(context.Context, string, string, string, []string, io.Reader, io.Writer, io.Writer) error {
				return errors.New("Pending seed must not execute")
			}, config)
			request := testRequest("ci", greedyDocument)
			request.Name = "wf"
			request.RunID = "run"
			request.SHA = "sha"
			request.SourceDir = writeCheckout(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := seeder.SeedRequest(ctx, request, false); done <- err }()
			<-ready
			controller := &Controller{workflows: &schedulingWorkflowClient{err: apierrors.NewNotFound(schema.GroupResource{Resource: "workflows"}, "wf")}, seeder: seeder, config: config}
			var observation *model.ExecutionObservation
			deadline := time.Now().Add(5 * time.Second)
			for {
				var err error
				observation, err = withTestObservationCache(t, controller).ObserveScheduling(t.Context(), "wf", "run", "sha")
				if err != nil {
					t.Fatal(err)
				}
				if observation.Reason == "SourceSeedPending" {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("seed observation unavailable", observation)
				}
				time.Sleep(time.Millisecond)
			}
			want := map[string]string{"image": "ImagePullBackOff", "claim": "unbound persistent volume claim", "capacity": "insufficient cpu"}[kind]
			if !strings.Contains(observation.Message, want) || strings.Contains(observation.Message, "PRIVATE") {
				t.Fatal("seed waiting reason missing or leaked raw text", observation)
			}
			if _, err := withTestObservationCache(t, controller).ObserveScheduling(t.Context(), "wf", "other-run", "sha"); err == nil {
				t.Fatal("foreign run observed seed")
			}
			pod, err := client.CoreV1().Pods(config.Namespace).Get(t.Context(), sourceClaimName("wf"), metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			original := pod.UID
			pod.UID = "foreign-seed"
			if _, err = client.CoreV1().Pods(config.Namespace).Update(t.Context(), pod, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			if _, err = withTestObservationCache(t, controller).ObserveScheduling(t.Context(), "wf", "run", "sha"); err == nil {
				t.Fatal("foreign Pod UID observed")
			}
			pod.UID = original
			client.CoreV1().Pods(config.Namespace).Update(t.Context(), pod, metav1.UpdateOptions{})
			select {
			case err := <-done:
				t.Fatal("native Pending failed", err)
			default:
			}
			cancel()
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			seeder.observationsMu.Lock()
			retained := len(seeder.observations)
			seeder.observationsMu.Unlock()
			if retained != 0 {
				t.Fatal("source identity observation retained")
			}
		})
	}
}

func TestSourceObservationCleanupDuringPodRead(t *testing.T) {
	config := seedTestConfig()
	client := fake.NewClientset()
	seeder := &SourceSeeder{client: client, config: config}
	stop := seeder.observeCreatedSeed(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "seed", UID: "uid"}}, &Request{Name: "wf", RunID: "run", SHA: "sha"})
	client.PrependReactor("get", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
		stop()
		return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "seed")
	})
	observation, err := seeder.observeScheduling(t.Context(), "wf", "run", "sha")
	if err != nil || observation != nil {
		t.Fatal("normal cleanup reported a disappeared seed", observation, err)
	}
}
