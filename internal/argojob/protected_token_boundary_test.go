package argojob

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	ktesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	"github.com/oberthci/oberth/internal/protectedtests"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

type protectedTokenJournal struct {
	ProtectedPlacementJournal
	admission *protectedtests.PlacementAdmission
	runtime   *protectedtests.GenericAdmission
}

func (j *protectedTokenJournal) PlacementAdmission(context.Context, string) (*protectedtests.PlacementAdmission, error) {
	return j.admission, nil
}
func (j *protectedTokenJournal) PlacementAdmissionForWorkflow(context.Context, string) (*protectedtests.PlacementAdmission, error) {
	return j.admission, nil
}
func (j *protectedTokenJournal) GenericAdmission(context.Context, string) (*protectedtests.GenericAdmission, error) {
	return j.runtime, nil
}
func (j *protectedTokenJournal) CheckGenericPreflightAuthority(context.Context, string) error {
	return nil
}
func (j *protectedTokenJournal) KVMPreflight(context.Context, string) (protectedtests.Execution, error) {
	return protectedtests.Execution{Receipt: &protectedtests.Receipt{}, Cleaned: true}, nil
}

func TestProtectedTokenDeliveryRetainsAuthorityWithExistingHolder(t *testing.T) {
	for _, mode := range []string{"unchanged", "removed config", "removed between attempts", "removed before delivery", "revoked between attempts", "expired between attempts"} {
		t.Run(mode, func(t *testing.T) {
			config, request := protectedPlacementFixture(t)
			_, _, objects := placementFixture(t)
			now := time.Now()
			runtimeAdmission := *request.GenericProtected
			runtimeAdmission.Intent.AdmittedAt = now
			runtimeAdmission.Intent.Deadline = now.Add(time.Minute)
			if mode == "expired between attempts" {
				expiry := now.Add(5 * time.Second)
				config.ExecutionPlacement.document.Expires = expiry
				config.ExecutionPlacement.digest = placementHash(config.ExecutionPlacement.document)
				runtimeAdmission.Intent.Deadline = expiry
			}
			request.GenericProtected = &runtimeAdmission
			request.TrustedKVM = &runtimeAdmission.Intent
			request = prepareTrustedTestRequest(t, config, request)

			admission, err := ProtectedAdmission(config, request)
			if err != nil {
				t.Fatal(err)
			}
			normal := request
			normal.Name = config.ExecutionPlacement.document.NormalWorkflow
			normal.TrustedKVM, normal.GenericProtected = nil, nil
			wf, err := Build(config, normal)
			if err != nil {
				t.Fatal(err)
			}
			wf.UID = "original-workflow-uid"
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "waiting-step", Namespace: wf.Namespace, UID: "waiting-uid", ResourceVersion: "1", Labels: map[string]string{"workflows.argoproj.io/workflow": wf.Name}, OwnerReferences: []metav1.OwnerReference{{UID: wf.UID, Kind: "Workflow", Controller: ptr.To(true)}}}, Spec: corev1.PodSpec{
				ServiceAccountName: config.PipelineServiceAccount, NodeName: config.ExecutionPlacement.document.NodeName, NodeSelector: config.ExecutionPlacement.selectors(),
				Volumes:        []corev1.Volume{{Name: executorTokenVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory}}}},
				InitContainers: []corev1.Container{{Name: executorTokenWaiter, Image: config.SourceSeedImage, Command: []string{"sh", "-c", executorTokenWaitCommand}, VolumeMounts: []corev1.VolumeMount{{Name: executorTokenVolume, MountPath: executorTokenDir}}}}, Containers: []corev1.Container{{Name: "main"}},
			}, Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{{Name: executorTokenWaiter, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}}}
			pod.Spec.Tolerations, err = config.ExecutionPlacement.admittedTolerations("candidate")
			if err != nil {
				t.Fatal(err)
			}
			objects = append(objects, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: request.Capability.ServiceAccount, Namespace: config.Namespace}}, pod, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "kube-root-ca.crt", Namespace: wf.Namespace}, Data: map[string]string{"ca.crt": "public-ca"}})
			kube := placementClient(objects...)
			kube.PrependReactor("create", "pods", func(a ktesting.Action) (bool, runtime.Object, error) {
				a.(ktesting.CreateAction).GetObject().(*corev1.Pod).UID = "identity-uid"
				return false, nil, nil
			})
			var c *Controller
			requests, deliveries := 0, 0
			revoked := false
			kube.PrependReactor("create", "serviceaccounts", func(a ktesting.Action) (bool, runtime.Object, error) {
				requests++
				if a.GetSubresource() != "token" {
					t.Fatal("unexpected service account create")
				}
				if requests == 1 && (mode == "removed between attempts" || mode == "removed before delivery") {
					c.config.ExecutionPlacement = nil
					if mode == "removed between attempts" {
						return true, nil, apierrors.NewTooManyRequests("bounded retry", 0)
					}
				}
				if requests == 1 && mode == "revoked between attempts" {
					revoked = true
					return true, nil, apierrors.NewTooManyRequests("bounded retry", 0)
				}
				if requests == 1 && mode == "expired between attempts" {
					timer := time.NewTimer(time.Until(config.ExecutionPlacement.document.Expires))
					defer timer.Stop()
					select {
					case <-timer.C:
					case <-t.Context().Done():
						t.Fatal(t.Context().Err())
					}
					return true, nil, apierrors.NewTooManyRequests("bounded retry", 0)
				}
				return true, &authenticationv1.TokenRequest{Status: authenticationv1.TokenRequestStatus{Token: "fixture-token", ExpirationTimestamp: metav1.NewTime(time.Now().Add(24 * time.Hour))}}, nil
			})
			c = &Controller{kube: kube, config: config, seeder: &SourceSeeder{exec: func(context.Context, string, string, string, []string, io.Reader, io.Writer, io.Writer) error {
				deliveries++
				return nil
			}}}
			c.WithProtectedPlacementJournal(&protectedTokenJournal{admission: admission, runtime: request.GenericProtected}, &rest.Config{Host: "https://api.example.org", TLSClientConfig: rest.TLSClientConfig{CAData: []byte("fixture-ca")}})
			c.WithProtectedPlacementAuthority(func(context.Context, protectedtests.GenericAdmission) error { return nil })
			c.WithWorkflowCapabilityLoader(func(context.Context, int64) (*argoworkflow.Capability, error) {
				if revoked {
					return nil, nil
				}
				return request.Capability, nil
			})
			if _, err = c.executorIdentityPod(t.Context(), wf, config.ExecutorServiceAccount); err != nil {
				t.Fatal(err)
			}
			if mode == "removed config" {
				c.config.ExecutionPlacement = nil
			}
			err = c.deliverExecutorTokensObserved(t.Context(), wf, map[types.UID]bool{}, nil)
			if mode == "unchanged" {
				if err != nil || requests != 1 || deliveries != 1 {
					t.Fatalf("unchanged delivery err=%v requests=%d deliveries=%d", err, requests, deliveries)
				}
			} else {
				expected := 1
				if mode == "removed config" {
					expected = 0
				}
				if err == nil || requests != expected || deliveries != 0 {
					t.Fatalf("authority bypass err=%v requests=%d deliveries=%d", err, requests, deliveries)
				}
			}
		})
	}
}

func TestProtectedSourceDeleteRefusesForeignOwnerAndReplacement(t *testing.T) {
	for _, mode := range []string{"original unowned", "foreign owner", "replacement UID", "removed config"} {
		t.Run(mode, func(t *testing.T) {
			config, _ := protectedPlacementFixture(t)
			claim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: config.ExecutionPlacement.document.PreflightWorkflow + "-src", Namespace: config.Namespace, UID: "original-source"}}
			if mode == "foreign owner" {
				claim.OwnerReferences = []metav1.OwnerReference{{APIVersion: "argoproj.io/v1alpha1", Kind: "Workflow", Name: "foreign", UID: "foreign"}}
			}
			if mode == "replacement UID" {
				claim.UID = "replacement"
			}
			kube := fake.NewClientset(claim)
			seeder := NewSourceSeeder(kube, func(context.Context, string, string, string, []string, io.Reader, io.Writer, io.Writer) error {
				return nil
			}, config)
			seeder.placementCheck = func(context.Context, string) error { return nil }
			seeder.placementSource = func(_ context.Context, _ string, uid string, _ bool) error {
				if uid != "original-source" {
					return errors.New("changed source UID")
				}
				return nil
			}
			if mode == "removed config" {
				seeder.config.ExecutionPlacement = nil
				seeder.placementCheck = func(context.Context, string) error {
					return errors.New("retained placed workflow cannot use removed config")
				}
			}
			seeder.DeleteClaim(t.Context(), claim.Name)
			_, err := kube.CoreV1().PersistentVolumeClaims(config.Namespace).Get(t.Context(), claim.Name, metav1.GetOptions{})
			if mode == "original unowned" {
				if !apierrors.IsNotFound(err) {
					t.Fatal("original unowned claim retained", err)
				}
			} else if err != nil {
				t.Fatal("foreign claim deleted", err)
			}
		})
	}
}
