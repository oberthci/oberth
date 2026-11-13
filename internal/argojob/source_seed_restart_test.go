package argojob

import (
	"context"
	"errors"
	"io"
	"testing"
	"testing/synctest"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestReusedSeedIsRecreatedOnceAfterRestartCleanup(t *testing.T) {
	for _, replacementDisappears := range []bool{false, true} {
		t.Run(map[bool]string{false: "running-replacement", true: "missing-replacement"}[replacementDisappears], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				config := seedTestConfig()
				config.SeedPollInterval = time.Second
				name := sourceClaimName("workflow")
				client := fake.NewClientset(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: config.Namespace, UID: "old"}, Status: corev1.PodStatus{Phase: corev1.PodPending}})
				creates, execs := 0, 0
				client.PrependReactor("create", "pods", func(action ktesting.Action) (bool, runtime.Object, error) {
					creates++
					pod := action.(ktesting.CreateAction).GetObject().(*corev1.Pod)
					pod.UID = "new"
					pod.Status.Phase = corev1.PodRunning
					if replacementDisappears {
						pod.Status.Phase = corev1.PodPending
					}
					return false, nil, nil
				})
				seeder := NewSourceSeeder(client, func(context.Context, string, string, string, []string, io.Reader, io.Writer, io.Writer) error {
					execs++
					return nil
				}, config)
				source := writeCheckout(t)
				done := make(chan error, 1)
				go func() { _, err := seeder.Seed(t.Context(), "workflow", source, false); done <- err }()
				synctest.Wait()
				if creates != 1 || execs != 0 {
					t.Fatal("did not reuse Pending seed", creates, execs)
				}
				if err := client.CoreV1().Pods(config.Namespace).Delete(t.Context(), name, metav1.DeleteOptions{}); err != nil {
					t.Fatal(err)
				}
				time.Sleep(time.Second)
				synctest.Wait()
				if replacementDisappears {
					if err := client.CoreV1().Pods(config.Namespace).Delete(t.Context(), name, metav1.DeleteOptions{}); err != nil {
						t.Fatal(err)
					}
					time.Sleep(time.Second)
					synctest.Wait()
				}
				err := <-done
				if replacementDisappears {
					if !apierrors.IsNotFound(err) || execs != 0 {
						t.Fatal("replacement loss was retried or executed", err, execs)
					}
				} else if err != nil || execs != 1 {
					t.Fatal("source was not copied", err, execs)
				}
				if creates != 2 {
					t.Fatal("must recreate only once", creates)
				}
				pods, err := client.CoreV1().Pods(config.Namespace).List(t.Context(), metav1.ListOptions{})
				if err != nil || len(pods.Items) != 0 {
					t.Fatal("seed leaked", pods, err)
				}
			})
		})
	}
}

func TestReusedSeedAlreadyGoneAtFirstReadIsRecreated(t *testing.T) {
	config := seedTestConfig()
	client := fake.NewClientset()
	creates := 0
	client.PrependReactor("create", "pods", func(action ktesting.Action) (bool, runtime.Object, error) {
		creates++
		pod := action.(ktesting.CreateAction).GetObject().(*corev1.Pod)
		if creates == 1 {
			return true, &corev1.Pod{}, apierrors.NewAlreadyExists(schema.GroupResource{Resource: "pods"}, pod.Name)
		}
		pod.Status.Phase = corev1.PodRunning
		return false, nil, nil
	})
	calls := []seedExecCall{}
	seeder := NewSourceSeeder(client, recordingExec(&calls), config)
	if _, err := seeder.Seed(t.Context(), "workflow", writeCheckout(t), false); err != nil {
		t.Fatal(err)
	}
	if creates != 2 || len(calls) != 1 {
		t.Fatal(creates, len(calls))
	}
}

func TestReusedSeedCancellationDoesNotRecreate(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	config := seedTestConfig()
	client := fake.NewClientset()
	var calls []seedExecCall
	seeder := NewSourceSeeder(client, recordingExec(&calls), config)
	if _, err := seeder.Seed(ctx, "workflow", writeCheckout(t), false); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
