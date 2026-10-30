package argojob

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestExecutorIdentityAPILoadBeforeExecDefersWithoutMinting(t *testing.T) {
	for _, stage := range []string{"pod_list", "identity_pod", "trust_anchor"} {
		t.Run(stage, func(t *testing.T) {
			controller, wf, pod, requests := executorRetryFixture(t)
			verb, resource := "get", "pods"
			if stage == "pod_list" {
				verb = "list"
			}
			if stage == "trust_anchor" {
				resource = "configmaps"
			}
			failed := false
			controller.kube.(*fake.Clientset).PrependReactor(verb, resource, func(action ktesting.Action) (bool, runtime.Object, error) {
				if stage == "identity_pod" && action.(ktesting.GetAction).GetName() == pod.Name {
					return false, nil, nil
				}
				if !failed {
					failed = true
					return true, nil, apierrors.NewServiceUnavailable("temporary")
				}
				return false, nil, nil
			})
			attempts := 0
			controller.seeder.exec = func(context.Context, string, string, string, []string, io.Reader, io.Writer, io.Writer) error {
				attempts++
				return nil
			}
			delivered := map[types.UID]bool{}
			if err := controller.deliverExecutorTokens(t.Context(), wf, delivered); err != nil || *requests != 0 || attempts != 0 || delivered[pod.UID] {
				t.Fatalf("transient pre-token failure aborted or minted: %v", err)
			}
			if err := controller.deliverExecutorTokens(t.Context(), wf, delivered); err != nil || *requests != 1 || attempts != 1 || !delivered[pod.UID] {
				t.Fatalf("next tick did not recover: %v", err)
			}
		})
	}
}

func TestExecutorIdentityTokenRequestHasOneBoundedRetry(t *testing.T) {
	for _, fail := range []error{apierrors.NewTooManyRequests("busy", 1), apierrors.NewServiceUnavailable("busy"), apierrors.NewTimeoutError("busy", 1)} {
		synctest.Test(t, func(t *testing.T) {
			controller, wf, _, requests := executorRetryFixture(t)
			calls := 0
			controller.kube.(*fake.Clientset).PrependReactor("create", "serviceaccounts", func(ktesting.Action) (bool, runtime.Object, error) {
				calls++
				if calls == 1 {
					return true, nil, fail
				}
				return false, nil, nil
			})
			controller.seeder.exec = func(context.Context, string, string, string, []string, io.Reader, io.Writer, io.Writer) error {
				return nil
			}
			if err := controller.deliverExecutorTokens(t.Context(), wf, map[types.UID]bool{}); err != nil || calls != 2 || *requests != 1 {
				t.Fatalf("TokenRequest did not recover once: %v calls=%d", err, calls)
			}
			calls = 0
			controller.kube.(*fake.Clientset).PrependReactor("create", "serviceaccounts", func(action ktesting.Action) (bool, runtime.Object, error) {
				calls++
				return true, nil, fail
			})
			if err := controller.deliverExecutorTokens(t.Context(), wf, map[types.UID]bool{}); err == nil || calls != 2 {
				t.Fatalf("TokenRequest retry was unbounded: %v calls=%d", err, calls)
			}
		})
	}
	synctest.Test(t, func(t *testing.T) {
		controller, wf, _, _ := executorRetryFixture(t)
		calls := 0
		controller.kube.(*fake.Clientset).PrependReactor("create", "serviceaccounts", func(action ktesting.Action) (bool, runtime.Object, error) {
			calls++
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "serviceaccounts"}, "account", errors.New("denied"))
		})
		if err := controller.deliverExecutorTokens(t.Context(), wf, map[types.UID]bool{}); err == nil || calls != 1 {
			t.Fatal("permission failure retried", err)
		}
	})
}

func TestExecutorIdentityFinalDeadlineCancellationReconcilesCommit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		controller, wf, pod, _ := executorRetryFixture(t)
		calls := 0
		controller.seeder.exec = func(ctx context.Context, _, _, _ string, _ []string, _ io.Reader, _, _ io.Writer) error {
			calls++
			<-ctx.Done()
			if calls == 3 {
				current := pod.DeepCopy()
				current.Status.InitContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}
				if _, err := controller.kube.CoreV1().Pods(wf.Namespace).Update(t.Context(), current, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			return context.Canceled // client-go can translate an expired stream to canceled.
		}
		delivered := map[types.UID]bool{}
		if err := controller.deliverExecutorTokens(t.Context(), wf, delivered); err != nil || calls != 3 || !delivered[pod.UID] {
			t.Fatalf("deadline cancellation/final commit lost: calls=%d err=%v", calls, err)
		}
	})
}

func TestExecutorIdentityPostExecRevalidatesAndRetiresBoundToken(t *testing.T) {
	for _, change := range []string{"uid", "owner", "waiter", "volume", "gone", "read-error"} {
		t.Run(change, func(t *testing.T) {
			controller, wf, pod, _ := executorRetryFixture(t)
			controller.seeder.exec = func(context.Context, string, string, string, []string, io.Reader, io.Writer, io.Writer) error {
				current := pod.DeepCopy()
				switch change {
				case "uid":
					current.UID = "new-uid"
				case "owner":
					current.OwnerReferences[0].UID = "foreign-owner"
				case "waiter":
					current.Spec.InitContainers[0].Image = "untrusted"
				case "volume":
					current.Spec.Volumes[0].EmptyDir.Medium = ""
				case "gone":
					return controller.kube.CoreV1().Pods(wf.Namespace).Delete(t.Context(), pod.Name, metav1.DeleteOptions{})
				case "read-error":
					controller.kube.(*fake.Clientset).PrependReactor("get", "pods", func(action ktesting.Action) (bool, runtime.Object, error) {
						if action.(ktesting.GetAction).GetName() == pod.Name {
							return true, nil, apierrors.NewServiceUnavailable("temporary")
						}
						return false, nil, nil
					})
				}
				_, err := controller.kube.CoreV1().Pods(wf.Namespace).Update(t.Context(), current, metav1.UpdateOptions{})
				return err
			}
			delivered := map[types.UID]bool{}
			err := controller.deliverExecutorTokens(t.Context(), wf, delivered)
			var failure *executorIdentityError
			if !errors.As(err, &failure) || failure.InfrastructureFailure() != (change == "read-error") || delivered[pod.UID] {
				t.Fatalf("post-exec identity uncertainty accepted/mislabelled: %v", err)
			}
			_, err = controller.kube.CoreV1().Pods(wf.Namespace).Get(t.Context(), controller.identityPodName(wf.UID, controller.config.ExecutorServiceAccount), metav1.GetOptions{})
			if !apierrors.IsNotFound(err) {
				t.Fatalf("bound token lifetime Pod retained: %v", err)
			}
		})
	}
}

func TestExecutorIdentityStdinRemainsImmutableAfterDelivery(t *testing.T) {
	controller, wf, _, _ := executorRetryFixture(t)
	var retained io.Reader
	controller.seeder.exec = func(_ context.Context, _, _, _ string, _ []string, input io.Reader, _, _ io.Writer) error {
		retained = input
		return nil
	}
	if err := controller.deliverExecutorTokens(t.Context(), wf, map[types.UID]bool{}); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(retained)
	if err != nil || string(data) != "private-token\n"+wf.Namespace+"\n"+base64.StdEncoding.EncodeToString([]byte("public-ca"))+"\n" {
		t.Fatal("stale stdin copier sees cleared/changed bytes", err)
	}
}

func TestExecutorIdentityMarkerCreatedDuringDeliveryIsNotClobbered(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	command := strings.Replace(executorIdentityDeliveryCommand, executorTokenDir, ".", 1)
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir, cmd.Stdin = dir, reader
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Wait until the receiver passed the initial marker test and blocks on stdin.
	for {
		stages, err := filepath.Glob(filepath.Join(dir, ".identity.*"))
		if err != nil {
			t.Fatal(err)
		}
		if len(stages) > 0 {
			break
		}
		select {
		case <-ctx.Done():
			_ = cmd.Wait()
			t.Fatal("receiver did not stage")
		case <-time.After(time.Millisecond):
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("already-committed"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(writer, "later-token\nnamespace\n"+base64.StdEncoding.EncodeToString([]byte("public-ca"))+"\n"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "token"))
	if err != nil || string(data) != "already-committed" {
		t.Fatal("concurrent marker overwritten", err)
	}
}
