package argojob

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func TestSourceSeedPendingBeyondOldStartupTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		config := seedTestConfig()
		config.SeedPollInterval = time.Minute
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "source", Namespace: config.Namespace}, Status: corev1.PodStatus{Phase: corev1.PodPending}}
		client := fake.NewClientset(pod)
		seeder := &SourceSeeder{client: client, config: config}
		result := make(chan error, 1)
		go func() { result <- seeder.waitForSeedPod(t.Context(), pod.Name) }()
		time.Sleep(4 * time.Minute)
		synctest.Wait()
		select {
		case err := <-result:
			t.Fatalf("ordinary Pending Pod expired: %v", err)
		default:
		}
		pod.Status.Phase = corev1.PodRunning
		if _, err := client.CoreV1().Pods(config.Namespace).UpdateStatus(t.Context(), pod, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		if err := <-result; err != nil {
			t.Fatal(err)
		}
	})
}

func TestCreatedSourceSeedDisappearanceAndTerminalStateFailOnlyPreparation(t *testing.T) {
	for _, state := range []string{"deleted", "failed", "succeeded"} {
		t.Run(state, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				config := seedTestConfig()
				config.SeedPollInterval = time.Second
				client := fake.NewClientset()
				client.PrependReactor("create", "pods", func(a ktesting.Action) (bool, runtime.Object, error) {
					a.(ktesting.CreateAction).GetObject().(*corev1.Pod).Status.Phase = corev1.PodPending
					return false, nil, nil
				})
				seeder := NewSourceSeeder(client, func(context.Context, string, string, string, []string, io.Reader, io.Writer, io.Writer) error {
					return errors.New("Pending seed must not execute")
				}, config)
				result := make(chan error, 1)
				go func() { _, err := seeder.Seed(t.Context(), "workflow", writeCheckout(t), false); result <- err }()
				synctest.Wait()
				pods, err := client.CoreV1().Pods(config.Namespace).List(t.Context(), metav1.ListOptions{})
				if err != nil || len(pods.Items) != 1 {
					t.Fatal(pods, err)
				}
				pod := &pods.Items[0]
				if state == "deleted" {
					err = client.CoreV1().Pods(config.Namespace).Delete(t.Context(), pod.Name, metav1.DeleteOptions{})
				} else {
					pod.Status.Phase = corev1.PodFailed
					if state == "succeeded" {
						pod.Status.Phase = corev1.PodSucceeded
					}
					_, err = client.CoreV1().Pods(config.Namespace).UpdateStatus(t.Context(), pod, metav1.UpdateOptions{})
				}
				if err != nil {
					t.Fatal(err)
				}
				time.Sleep(time.Second)
				synctest.Wait()
				err = <-result
				if err == nil || (state == "deleted" && !strings.Contains(err.Error(), "disappeared before source preparation completed")) || (state != "deleted" && !strings.Contains(err.Error(), "ended in phase")) {
					t.Fatal("seed failed without a clear reason", err)
				}
				var refusal *argoworkflow.AdmissionError
				if errors.As(err, &refusal) {
					t.Fatal("seed infrastructure failure called YAML validation", err)
				}
				pods, err = client.CoreV1().Pods(config.Namespace).List(t.Context(), metav1.ListOptions{})
				if err != nil || len(pods.Items) != 0 {
					t.Fatal("seed retained", pods, err)
				}
				claims, err := client.CoreV1().PersistentVolumeClaims(config.Namespace).List(t.Context(), metav1.ListOptions{})
				if err != nil || len(claims.Items) != 0 {
					t.Fatal("volume retained", claims, err)
				}
			})
		})
	}
}
