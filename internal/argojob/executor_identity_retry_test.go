package argojob

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	kexec "k8s.io/client-go/util/exec"
	"k8s.io/utils/ptr"
)

func executorRetryFixture(t *testing.T) (*Controller, *wfv1.Workflow, *corev1.Pod, *int) {
	t.Helper()
	config := testConfig()
	config.applyDefaults()
	wf := &wfv1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: "identity-retry", Namespace: config.Namespace, UID: "workflow-uid", Annotations: map[string]string{runIDAnnotation: "identity-run", identityAnnotation: "spec-digest"}}, Spec: wfv1.WorkflowSpec{ActiveDeadlineSeconds: ptr.To(int64(300))}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "identity-step", Namespace: wf.Namespace, UID: "step-uid", Labels: map[string]string{"workflows.argoproj.io/workflow": wf.Name}, OwnerReferences: []metav1.OwnerReference{{UID: wf.UID, Kind: "Workflow", Controller: ptr.To(true)}}}, Spec: corev1.PodSpec{
		Volumes:        []corev1.Volume{{Name: executorTokenVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory}}}},
		InitContainers: []corev1.Container{{Name: executorTokenWaiter, Image: config.SourceSeedImage, Command: []string{"sh", "-c", executorTokenWaitCommand}, VolumeMounts: []corev1.VolumeMount{{Name: executorTokenVolume, MountPath: executorTokenDir}}}},
		Containers:     []corev1.Container{{Name: "main"}},
	}, Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{{Name: executorTokenWaiter, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}}}
	kube := fake.NewClientset(pod, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "kube-root-ca.crt", Namespace: wf.Namespace}, Data: map[string]string{"ca.crt": "public-ca"}})
	kube.PrependReactor("create", "pods", func(action ktesting.Action) (bool, runtime.Object, error) {
		action.(ktesting.CreateAction).GetObject().(*corev1.Pod).UID = "holder-uid"
		return false, nil, nil
	})
	requests := new(int)
	kube.PrependReactor("create", "serviceaccounts", func(action ktesting.Action) (bool, runtime.Object, error) {
		*requests++
		request := action.(ktesting.CreateAction).GetObject().(*authenticationv1.TokenRequest)
		if request.Spec.BoundObjectRef.UID != "holder-uid" {
			t.Error("token lost its lifetime Pod binding")
		}
		return true, &authenticationv1.TokenRequest{Status: authenticationv1.TokenRequestStatus{Token: "private-token", ExpirationTimestamp: metav1.NewTime(time.Now().Add(time.Hour))}}, nil
	})
	controller := &Controller{kube: kube, config: config, seeder: &SourceSeeder{}}
	return controller, wf, pod, requests
}

func TestExecutorIdentityTransientExecRetryReusesOneTokenAndPayload(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		controller, wf, pod, requests := executorRetryFixture(t)
		attempts := 0
		var body string
		controller.seeder.exec = func(ctx context.Context, _, _, container string, command []string, input io.Reader, _, _ io.Writer) error {
			attempts++
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) != executorIdentityAttemptTimeout {
				t.Fatal("attempt lacks a bounded deadline")
			}
			payload, _ := io.ReadAll(input)
			if body == "" {
				body = string(payload)
			} else if string(payload) != body {
				t.Fatal("retry minted or changed credentials")
			}
			if container != executorTokenWaiter || strings.Contains(strings.Join(command, " "), "private-token") {
				t.Fatal("identity delivery escaped stdin/waiter")
			}
			if attempts == 1 {
				return io.ErrUnexpectedEOF
			}
			if attempts == 2 {
				return apierrors.NewServiceUnavailable("private-token response text must not be logged")
			}
			return nil
		}
		started := time.Now()
		delivered := map[types.UID]bool{}
		if err := controller.deliverExecutorTokens(t.Context(), wf, delivered); err != nil {
			t.Fatal(err)
		}
		if attempts != 3 || *requests != 1 || !delivered[pod.UID] || time.Since(started) != 3*time.Second {
			t.Fatalf("retry budget/credentials: attempts=%d requests=%d elapsed=%s", attempts, *requests, time.Since(started))
		}
	})
}

func TestExecutorIdentityExecDeadlineBudgetAndSafeError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		controller, wf, _, requests := executorRetryFixture(t)
		attempts := 0
		controller.seeder.exec = func(ctx context.Context, _, _, _ string, _ []string, _ io.Reader, _, _ io.Writer) error {
			attempts++
			<-ctx.Done()
			return ctx.Err()
		}
		started := time.Now()
		err := controller.deliverExecutorTokens(t.Context(), wf, map[types.UID]bool{})
		var classified *executorIdentityError
		if !errors.As(err, &classified) || !classified.InfrastructureFailure() || !errors.Is(err, context.DeadlineExceeded) || classified.attempts != 3 {
			t.Fatalf("lost infrastructure classification/cause: %v", err)
		}
		if attempts != 3 || *requests != 1 || time.Since(started) != 63*time.Second {
			t.Fatalf("unbounded retry: attempts=%d requests=%d elapsed=%s", attempts, *requests, time.Since(started))
		}
		safe := identityFailure("exec", errors.New("private-token arbitrary response"))
		if strings.Contains(safe.Error(), "private-token") {
			t.Fatal("error included credential-bearing response")
		}
	})
}

func TestExecutorIdentityPermanentExecErrorsDoNotRetry(t *testing.T) {
	for name, err := range map[string]error{
		"forbidden":    apierrors.NewForbidden(schema.GroupResource{Resource: "pods/exec"}, "step", errors.New("denied")),
		"unauthorized": apierrors.NewUnauthorized("denied"),
		"invalid":      apierrors.NewBadRequest("bad request"),
		"exit":         kexec.CodeExitError{Err: errors.New("script failed"), Code: 1},
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				controller, wf, _, _ := executorRetryFixture(t)
				attempts := 0
				controller.seeder.exec = func(context.Context, string, string, string, []string, io.Reader, io.Writer, io.Writer) error {
					attempts++
					return err
				}
				failure := controller.deliverExecutorTokens(t.Context(), wf, map[types.UID]bool{})
				if !errors.Is(failure, err) || attempts != 1 {
					t.Fatalf("permanent error retried or lost: attempts=%d err=%v", attempts, failure)
				}
			})
		})
	}
}

func TestExecutorIdentityRetryRevalidatesTargetAndReconcilesLostAck(t *testing.T) {
	for _, scenario := range []string{"new-uid", "foreign-owner", "modified-waiter", "token-committed"} {
		t.Run(scenario, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				controller, wf, pod, requests := executorRetryFixture(t)
				attempts := 0
				controller.seeder.exec = func(context.Context, string, string, string, []string, io.Reader, io.Writer, io.Writer) error {
					attempts++
					current := pod.DeepCopy()
					switch scenario {
					case "new-uid":
						current.UID = "replacement-uid"
					case "foreign-owner":
						current.OwnerReferences[0].UID = "foreign-workflow"
					case "modified-waiter":
						current.Spec.InitContainers[0].Image = "untrusted"
					case "token-committed":
						current.Status.InitContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}
					}
					if _, err := controller.kube.CoreV1().Pods(wf.Namespace).Update(t.Context(), current, metav1.UpdateOptions{}); err != nil {
						t.Fatal(err)
					}
					return io.ErrUnexpectedEOF
				}
				delivered := map[types.UID]bool{}
				err := controller.deliverExecutorTokens(t.Context(), wf, delivered)
				if attempts != 1 || *requests != 1 {
					t.Fatal("replayed delivery after target changed or waiter completed")
				}
				if scenario == "token-committed" {
					if err != nil || !delivered[pod.UID] {
						t.Fatal("lost acknowledgement was not reconciled", err)
					}
				} else if err == nil || delivered[pod.UID] {
					t.Fatal("untrusted replacement was accepted")
				}
			})
		})
	}
}

func TestExecutorIdentityRetryHandlesReadErrorsAndCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		controller, wf, pod, _ := executorRetryFixture(t)
		reads, attempts := 0, 0
		controller.kube.(*fake.Clientset).PrependReactor("get", "pods", func(action ktesting.Action) (bool, runtime.Object, error) {
			if action.(ktesting.GetAction).GetName() != pod.Name {
				return false, nil, nil
			}
			reads++
			if reads == 1 {
				return true, nil, apierrors.NewTooManyRequests("busy", 1)
			}
			return false, nil, nil
		})
		controller.seeder.exec = func(context.Context, string, string, string, []string, io.Reader, io.Writer, io.Writer) error {
			attempts++
			return nil
		}
		if err := controller.deliverExecutorTokens(t.Context(), wf, map[types.UID]bool{}); err != nil || attempts != 1 || reads != 2 {
			t.Fatalf("transient target read ended run: %v", err)
		}
		// A short parent deadline cancels the in-flight attempt; no new attempt or
		// detached goroutine may outlive scheduler cancellation.
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		controller.seeder.exec = func(ctx context.Context, _, _, _ string, _ []string, _ io.Reader, _, _ io.Writer) error {
			attempts++
			<-ctx.Done()
			return ctx.Err()
		}
		started := time.Now()
		err := controller.deliverExecutorTokens(ctx, wf, map[types.UID]bool{})
		if !errors.Is(err, context.DeadlineExceeded) || attempts != 2 || time.Since(started) != time.Second {
			t.Fatalf("cancellation retried or exceeded its budget: attempts=%d elapsed=%s err=%v", attempts, time.Since(started), err)
		}
	})
}

func TestExecutorIdentityWaitSurvivesTransientDelivery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		controller, wf, _, _ := executorRetryFixture(t)
		wf.Status.Phase = wfv1.WorkflowRunning
		controller.workflows = &staticWorkflowClient{workflow: wf}
		controller.interval = time.Millisecond
		attempts := 0
		controller.seeder.exec = func(context.Context, string, string, string, []string, io.Reader, io.Writer, io.Writer) error {
			attempts++
			if attempts == 1 {
				return io.ErrUnexpectedEOF
			}
			wf.Status.Phase = wfv1.WorkflowSucceeded
			return nil
		}
		result, err := controller.Wait(t.Context(), wf.Name, "identity-run", io.Discard)
		if err != nil || !result.Succeeded || attempts != 2 {
			t.Fatalf("transient delivery terminalized Wait: %+v %v", result, err)
		}
	})
}

func TestExecutorIdentityDeliveryCommandIsAtomicAndReplaySafe(t *testing.T) {
	command := strings.Replace(executorIdentityDeliveryCommand, executorTokenDir, ".", 1)
	invoke := func(dir, payload string) error {
		cmd := exec.Command("sh", "-c", command)
		cmd.Dir, cmd.Stdin = dir, strings.NewReader(payload)
		return cmd.Run()
	}
	incomplete := t.TempDir()
	if err := invoke(incomplete, "incomplete-token"); err == nil {
		t.Fatal("truncated delivery succeeded")
	}
	if _, err := os.Stat(filepath.Join(incomplete, "token")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("partial delivery published the marker")
	}
	dir := t.TempDir()
	var workers sync.WaitGroup
	failures := make(chan error, 8)
	for range 8 {
		workers.Go(func() { failures <- invoke(dir, "first-token\nnamespace\npublic-ca") })
	}
	workers.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal("concurrent retry failed", err)
		}
	}
	if err := invoke(dir, "replacement-token\nother-namespace\nother-ca"); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"token": "first-token", "namespace": "namespace", "ca.crt": "public-ca"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(data) != want {
			t.Fatalf("replay changed committed %s: %q %v", name, data, err)
		}
	}
	stages, err := filepath.Glob(filepath.Join(dir, ".identity.*"))
	if err != nil || len(stages) != 0 {
		t.Fatal("staging files retained credentials", stages, err)
	}
}
