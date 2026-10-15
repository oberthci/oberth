package argojob

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"
)

func TestTrustedKVMRecoveryRequiresContinuousRequestTimeoutAbsence(t *testing.T) {
	if trustedCreateAbsenceWindow < 60*time.Second {
		t.Fatal("absence proof shorter than API timeout")
	}
	for _, mode := range []string{"absent", "slow cleanup", "late workflow", "claim", "mounted claim", "seed pod", "API error", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			config, request, wf, _ := trustedKVMObservedFixture(t)
			workflows := &trustedCleanupClient{workflow: wf, deleted: true}
			kube := trustedControllerClient()
			controller, err := NewController(workflows, kube, config)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			start := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
			now := start
			observations := 0
			err = controller.observeTrustedKVMAbsence(ctx, request.Name, func() time.Time { return now }, func(context.Context) error {
				observations++
				if observations == 90 {
					cancel()
				}
				if mode == "slow cleanup" {
					if observations == 1 {
						if _, e := kube.CoreV1().PersistentVolumeClaims(config.Namespace).Create(ctx, &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: sourceClaimName(request.Name)}}, metav1.CreateOptions{}); e != nil {
							t.Fatal(e)
						}
					}
					if observations == 10 {
						if e := kube.CoreV1().PersistentVolumeClaims(config.Namespace).Delete(ctx, sourceClaimName(request.Name), metav1.DeleteOptions{}); e != nil {
							t.Fatal(e)
						}
					}
				}
				now = now.Add(time.Second)
				if observations == 59 {
					switch mode {
					case "late workflow":
						workflows.deleted = false
					case "claim":
						_, e := kube.CoreV1().PersistentVolumeClaims(config.Namespace).Create(ctx, &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: sourceClaimName(request.Name)}}, metav1.CreateOptions{})
						if e != nil {
							t.Fatal(e)
						}
					case "mounted claim":
						_, e := kube.CoreV1().Pods(config.Namespace).Create(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "consumer"}, Spec: corev1.PodSpec{Volumes: []corev1.Volume{{Name: "source", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: sourceClaimName(request.Name)}}}}}}, metav1.CreateOptions{})
						if e != nil {
							t.Fatal(e)
						}
					case "seed pod":
						_, e := kube.CoreV1().Pods(config.Namespace).Create(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: sourceClaimName(request.Name)}}, metav1.CreateOptions{})
						if e != nil {
							t.Fatal(e)
						}
					case "API error":
						kube.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, errors.New("API unavailable") })
					case "canceled":
						cancel()
					}
				}
				return nil
			})
			if mode == "absent" || mode == "slow cleanup" {
				want := 60
				if mode == "slow cleanup" {
					want = 70
				}
				if err != nil || now.Sub(start) != time.Duration(want)*time.Second || observations != want {
					t.Fatalf("absence window: %v elapsed=%s observations=%d", err, now.Sub(start), observations)
				}
			} else if err == nil {
				t.Fatal("uncertain absence accepted")
			}
		})
	}
}

type rejectingTrustedWorkflowClient struct {
	*trustedCleanupClient
	createErr error
	attempted bool
}

func (c *rejectingTrustedWorkflowClient) Create(context.Context, *wfv1.Workflow, metav1.CreateOptions) (*wfv1.Workflow, error) {
	c.attempted = true
	return nil, c.createErr
}

func TestTrustedKVMExplicitPOSTRefusalOnly(t *testing.T) {
	resource := schema.GroupResource{Group: "argoproj.io", Resource: "workflows"}
	for _, test := range []struct {
		name     string
		err      error
		definite bool
	}{
		{"invalid", apierrors.NewInvalid(schema.GroupKind{Group: "argoproj.io", Kind: "Workflow"}, "test", nil), true},
		{"forbidden", apierrors.NewForbidden(resource, "test", errors.New("denied")), true},
		{"bad request", apierrors.NewBadRequest("denied"), true},
		{"timeout", apierrors.NewTimeoutError("timeout", 1), false},
		{"server", apierrors.NewInternalError(errors.New("server failure")), false},
		{"transport", errors.New("connection reset"), false},
		{"already exists", apierrors.NewAlreadyExists(resource, "test"), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			config, request, wf, _ := trustedKVMObservedFixture(t)
			request.SourceDir = writeCheckout(t)
			request = prepareTrustedTestRequest(t, config, request)
			workflows := &rejectingTrustedWorkflowClient{trustedCleanupClient: &trustedCleanupClient{workflow: wf, deleted: true}, createErr: test.err}
			kube := trustedControllerClient(&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: wf.Spec.ServiceAccountName, Namespace: config.Namespace}})
			runningSeedPods(kube)
			config.SeedPollInterval = time.Millisecond
			controller, err := NewController(workflows, kube, config)
			if err != nil {
				t.Fatal(err)
			}
			controller.WithSourceSeeder(NewSourceSeeder(kube, func(_ context.Context, _, _, _ string, _ []string, in io.Reader, _, _ io.Writer) error {
				_, e := io.Copy(io.Discard, in)
				return e
			}, config))
			_, err = controller.Create(context.Background(), request)
			if !workflows.attempted || errors.Is(err, ErrTrustedKVMCreateRefused) != test.definite {
				t.Fatalf("POST classification attempted=%v definite=%v err=%v", workflows.attempted, test.definite, err)
			}
		})
	}
}
