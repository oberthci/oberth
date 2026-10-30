package argojob

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

func evictedControllerFixture() (*corev1.Pod, *corev1.Pod, *corev1.ConfigMap, *appsv1.ReplicaSet) {
	live, cm := nonrootVerifierObjects()
	cm.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Minute))
	live.Status.ContainerStatuses[0].State.Running.StartedAt = metav1.NewTime(time.Now().Add(-10 * time.Second))
	live.OwnerReferences[0].APIVersion = "apps/v1"
	live.CreationTimestamp = metav1.NewTime(cm.CreationTimestamp.Add(time.Second))
	live.ResourceVersion = "10"
	old := live.DeepCopy()
	old.Name = "evicted-controller"
	old.UID = "evicted-uid"
	old.ResourceVersion = "9"
	old.CreationTimestamp = cm.CreationTimestamp
	old.Status = corev1.PodStatus{Phase: corev1.PodFailed, Reason: "Evicted", Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}, {Type: corev1.ContainersReady, Status: corev1.ConditionFalse}}, ContainerStatuses: []corev1.ContainerStatus{{Name: "controller", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "ContainerStatusUnknown", ExitCode: 137}}}}}
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "controller", Namespace: testNamespace, UID: "replicaset-uid", ResourceVersion: "20", Generation: 1, OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: "controller", UID: "deployment-uid", Controller: ptr.To(true)}}}, Spec: appsv1.ReplicaSetSpec{Replicas: ptr.To(int32(1)), Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "argo-workflows-workflow-controller"}}}, Status: appsv1.ReplicaSetStatus{ObservedGeneration: 1, Replicas: 1, ReadyReplicas: 1}}
	return live, old, cm, rs
}
func TestControllerEvictedHistoryReadyReplacementRestoresProof(t *testing.T) {
	for _, mode := range []string{"unknown-exit", "missing-status", "node-shutdown", "graceful-node-shutdown", "scaled-zero-predecessor"} {
		t.Run(mode, func(t *testing.T) {
			live, old, cm, rs := evictedControllerFixture()
			objects := []runtime.Object{live, old, cm, rs}
			if mode == "node-shutdown" {
				old.Status.Reason = "NodeShutdown"
			}
			if mode == "graceful-node-shutdown" {
				old.Status.Reason = "Terminated"
				old.Status.Message = "Pod was terminated in response to imminent node shutdown."
			}
			if mode == "missing-status" {
				old.Status.ContainerStatuses = nil
			}
			if mode == "scaled-zero-predecessor" {
				previous := rs.DeepCopy()
				previous.Name = "old-controller"
				previous.UID = "old-rs"
				previous.Spec.Replicas = ptr.To(int32(0))
				previous.Status.Replicas = 0
				previous.Status.ReadyReplicas = 0
				old.OwnerReferences[0].Name = previous.Name
				old.OwnerReferences[0].UID = previous.UID
				objects = append(objects, previous)
			}
			client := fake.NewClientset(objects...)
			proof, err := verifyNonrootController(t.Context(), client, testNamespace, testRequest(periapsis.TriggerCI, nonrootTestDocument))
			if err != nil || proof == nil || proof.podUID != string(live.UID) {
				t.Fatal("verified replacement remained blocked or history supplied authority", proof, err)
			}
		})
	}
}
func TestControllerEvictedHistoryRejectsAmbiguousReplacement(t *testing.T) {
	cases := map[string]func(*corev1.Pod, *corev1.Pod, *corev1.ConfigMap, *appsv1.ReplicaSet){
		"ready-deficit": func(_, _ *corev1.Pod, _ *corev1.ConfigMap, r *appsv1.ReplicaSet) { r.Status.ReadyReplicas = 0 },
		"desired-two": func(_, _ *corev1.Pod, _ *corev1.ConfigMap, r *appsv1.ReplicaSet) {
			r.Spec.Replicas = ptr.To(int32(2))
			r.Status.ReadyReplicas = 2
			r.Status.Replicas = 2
		},
		"old-generation": func(_, _ *corev1.Pod, _ *corev1.ConfigMap, r *appsv1.ReplicaSet) { r.Generation = 2 },
		"wrong-rs-uid":   func(_, _ *corev1.Pod, _ *corev1.ConfigMap, r *appsv1.ReplicaSet) { r.UID = "foreign" },
		"empty-selector": func(_, _ *corev1.Pod, _ *corev1.ConfigMap, r *appsv1.ReplicaSet) {
			r.Spec.Selector = &metav1.LabelSelector{}
		},
		"unmatched-selector": func(_, _ *corev1.Pod, _ *corev1.ConfigMap, r *appsv1.ReplicaSet) {
			r.Spec.Selector.MatchLabels = map[string]string{"other": "controller"}
		},
		"deleting-rs": func(_, _ *corev1.Pod, _ *corev1.ConfigMap, r *appsv1.ReplicaSet) {
			r.DeletionTimestamp = ptr.To(metav1.Now())
		},
		"foreign-history-owner": func(_, p *corev1.Pod, _ *corev1.ConfigMap, _ *appsv1.ReplicaSet) {
			p.OwnerReferences[0].UID = "foreign"
		},
		"uncontrolled-history": func(_, p *corev1.Pod, _ *corev1.ConfigMap, _ *appsv1.ReplicaSet) {
			p.OwnerReferences[0].Controller = ptr.To(false)
		},
		"extra-history-owner": func(_, p *corev1.Pod, _ *corev1.ConfigMap, _ *appsv1.ReplicaSet) {
			p.OwnerReferences = append(p.OwnerReferences, p.OwnerReferences[0])
		},
		"wrong-owner-api": func(_, p *corev1.Pod, _ *corev1.ConfigMap, _ *appsv1.ReplicaSet) {
			p.OwnerReferences[0].APIVersion = "other/v1"
		},
		"later-history": func(l, p *corev1.Pod, _ *corev1.ConfigMap, _ *appsv1.ReplicaSet) {
			p.CreationTimestamp = metav1.NewTime(l.CreationTimestamp.Add(time.Second))
		},
		"missing-created": func(_, p *corev1.Pod, _ *corev1.ConfigMap, _ *appsv1.ReplicaSet) { p.CreationTimestamp = metav1.Time{} },
		"missing-version": func(_, p *corev1.Pod, _ *corev1.ConfigMap, _ *appsv1.ReplicaSet) { p.ResourceVersion = "" },
		"running-history-container": func(_, p *corev1.Pod, _ *corev1.ConfigMap, _ *appsv1.ReplicaSet) {
			p.Status.ContainerStatuses[0].State.Running = &corev1.ContainerStateRunning{}
		},
		"ready-history": func(_, p *corev1.Pod, _ *corev1.ConfigMap, _ *appsv1.ReplicaSet) {
			p.Status.ContainerStatuses[0].Ready = true
		},
		"ready-history-condition": func(_, p *corev1.Pod, _ *corev1.ConfigMap, _ *appsv1.ReplicaSet) {
			p.Status.Conditions[0].Status = corev1.ConditionTrue
		},
		"duplicate-history-condition": func(_, p *corev1.Pod, _ *corev1.ConfigMap, _ *appsv1.ReplicaSet) {
			p.Status.Conditions = append(p.Status.Conditions, p.Status.Conditions[0])
		},
		"non-eviction-failure": func(_, p *corev1.Pod, _ *corev1.ConfigMap, _ *appsv1.ReplicaSet) { p.Status.Reason = "Error" },
		"extra-live-authority": func(_, p *corev1.Pod, _ *corev1.ConfigMap, _ *appsv1.ReplicaSet) { p.Status.Phase = corev1.PodRunning },
		"wrong-live-profile": func(l, _ *corev1.Pod, _ *corev1.ConfigMap, _ *appsv1.ReplicaSet) {
			l.Spec.Containers[0].Image = "untrusted-image"
		},
		"wrong-config": func(_, _ *corev1.Pod, c *corev1.ConfigMap, _ *appsv1.ReplicaSet) {
			c.Data["config"] = "untrusted-config"
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			live, old, cm, rs := evictedControllerFixture()
			change(live, old, cm, rs)
			if proof, err := verifyNonrootController(t.Context(), fake.NewClientset(live, old, cm, rs), testNamespace, testRequest(periapsis.TriggerCI, nonrootTestDocument)); err == nil || proof != nil {
				t.Fatal("ambiguous replacement authorized submission", proof, err)
			}
		})
	}
}
func TestControllerEvictedHistoryPredecessorMustBeDrainedSameDeployment(t *testing.T) {
	for _, mode := range []string{"still-desired", "still-ready", "foreign-deployment", "missing-deployment"} {
		t.Run(mode, func(t *testing.T) {
			live, old, cm, rs := evictedControllerFixture()
			previous := rs.DeepCopy()
			previous.Name = "old-controller"
			previous.UID = "old-rs"
			previous.Spec.Replicas = ptr.To(int32(0))
			previous.Status.Replicas = 0
			previous.Status.ReadyReplicas = 0
			old.OwnerReferences[0].Name = previous.Name
			old.OwnerReferences[0].UID = previous.UID
			switch mode {
			case "still-desired":
				previous.Spec.Replicas = ptr.To(int32(1))
			case "still-ready":
				previous.Status.Replicas = 1
				previous.Status.ReadyReplicas = 1
			case "foreign-deployment":
				previous.OwnerReferences[0].UID = "foreign"
			case "missing-deployment":
				previous.OwnerReferences = nil
			}
			if proof, err := verifyNonrootController(t.Context(), fake.NewClientset(live, old, cm, rs, previous), testNamespace, testRequest(periapsis.TriggerCI, nonrootTestDocument)); err == nil || proof != nil {
				t.Fatal("foreign/live predecessor ignored", proof, err)
			}
		})
	}
}
func TestControllerEvictedHistoryIncompleteListAndRSReadFailClosed(t *testing.T) {
	for _, mode := range []string{"incomplete-list", "read-error"} {
		t.Run(mode, func(t *testing.T) {
			live, old, cm, rs := evictedControllerFixture()
			client := fake.NewClientset(live, old, cm, rs)
			if mode == "read-error" {
				client.PrependReactor("get", "replicasets", func(ktesting.Action) (bool, runtime.Object, error) { return true, nil, context.DeadlineExceeded })
			} else {
				client.PrependReactor("list", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
					return true, &corev1.PodList{ListMeta: metav1.ListMeta{Continue: "next"}, Items: []corev1.Pod{*live, *old}}, nil
				})
			}
			proof, err := verifyNonrootController(t.Context(), client, testNamespace, testRequest(periapsis.TriggerCI, nonrootTestDocument))
			if proof != nil || err == nil {
				t.Fatal("missing observation authorized admission", proof, err)
			}
			if mode == "read-error" {
				var read *profileReadError
				if !errors.As(err, &read) {
					t.Fatal("RS API failure lost retry class", err)
				}
			}
		})
	}
}
func TestControllerEvictedHistoryAdmissionPauseIsObservableAndRecovers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		live, old, cm, rs := evictedControllerFixture()
		rs.Status.ReadyReplicas = 0
		client := fake.NewClientset(live, old, cm, rs)
		config := testConfig()
		config.NonrootProfile = argoworkflow.NonrootStaticProfile
		c, err := NewController(&staticWorkflowClient{}, client, config)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.PrepareNonroot(t.Context(), testRequest(periapsis.TriggerCI, nonrootTestDocument)); err == nil {
			t.Fatal("unready replacement accepted")
		}
		status := c.ControllerProfileStatus()
		if !status.AdmissionPaused || status.Remedy == "" || !strings.Contains(status.Reason, "ReplicaSet") {
			t.Fatal("pause invisible", status)
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- c.AwaitAdmission(ctx) }()
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatal("pause bypassed", err)
		default:
		}
		rs.Status.ReadyReplicas = 1
		if _, err := client.AppsV1().ReplicaSets(testNamespace).UpdateStatus(t.Context(), rs, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		status = c.ControllerProfileStatus()
		if status.AdmissionPaused || status.State != "verified" || status.Remedy != "" || status.Reason != "" {
			t.Fatal("pause did not clear", status)
		}
	})
}

func TestControllerNodeShutdownRequiresReadyReplacement(t *testing.T) {
	for _, reason := range []string{"NodeShutdown", "Terminated"} {
		for _, ready := range []int32{0, 1} {
			t.Run(reason+"/"+map[int32]string{0: "unready", 1: "ready"}[ready], func(t *testing.T) {
				live, old, cm, rs := evictedControllerFixture()
				old.Status.Reason = reason
				if reason == "Terminated" {
					old.Status.Message = "Pod was terminated in response to imminent node shutdown."
				}
				rs.Status.ReadyReplicas = ready
				proof, err := verifyNonrootController(t.Context(), fake.NewClientset(live, old, cm, rs), testNamespace, testRequest(periapsis.TriggerCI, nonrootTestDocument))
				if ready == 0 && (err == nil || proof != nil) {
					t.Fatal("shutdown bypassed Ready replacement proof", proof, err)
				}
				if ready == 1 && (err != nil || proof == nil) {
					t.Fatal("verified shutdown replacement remained blocked", proof, err)
				}
			})
		}
	}
}

func TestControllerTerminatedHistoryNeedsShutdownMessage(t *testing.T) {
	for _, message := range []string{"", "some other termination"} {
		t.Run(message, func(t *testing.T) {
			live, old, cm, rs := evictedControllerFixture()
			old.Status.Reason = "Terminated"
			old.Status.Message = message
			proof, err := verifyNonrootController(t.Context(), fake.NewClientset(live, old, cm, rs), testNamespace, testRequest(periapsis.TriggerCI, nonrootTestDocument))
			if err == nil || proof != nil {
				t.Fatal("generic Terminated Pod bypassed history verification", proof, err)
			}
		})
	}
}
