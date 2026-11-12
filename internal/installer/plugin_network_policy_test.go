package installer

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/oberthci/oberth/internal/containerimage"
)

func TestPluginNetworkPolicyLiveProbeAndCleanup(t *testing.T) {
	for _, mode := range []string{"enforced", "absent", "namespace forbidden", "image unavailable", "exec forbidden", "cancelled", "cleanup forbidden", "create response lost"} {
		t.Run(mode, func(t *testing.T) {
			kube := fake.NewClientset(preflightNode())
			namespace := ""
			createdPods := 0
			cleanupSeen := false
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			kube.PrependReactor("create", "namespaces", func(action k8stesting.Action) (bool, runtime.Object, error) {
				obj := action.(k8stesting.CreateAction).GetObject().(*corev1.Namespace)
				namespace = obj.Name
				if mode == "namespace forbidden" {
					return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, obj.Name, errors.New("private diagnostic"))
				}
				obj.UID = "fixture-uid"
				if mode == "cancelled" || mode == "create response lost" {
					if err := kube.Tracker().Create(corev1.SchemeGroupVersion.WithResource("namespaces"), obj, ""); err != nil {
						t.Fatal(err)
					}
					if mode == "cancelled" {
						cancel()
					}
					return true, nil, errors.New("lost response")
				}
				return false, nil, nil
			})
			kube.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
				createdPods++
				pod := action.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
				if pod.Spec.Containers[0].Image != containerimage.Utility || pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken || !*pod.Spec.SecurityContext.RunAsNonRoot {
					t.Fatal("probe image or Pod isolation changed")
				}
				if mode != "image unavailable" {
					pod.Status = corev1.PodStatus{PodIP: "192.0.2.10", Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}
				}
				return false, nil, nil
			})
			kube.PrependReactor("delete", "namespaces", func(action k8stesting.Action) (bool, runtime.Object, error) {
				cleanupSeen = true

				deletion := action.(k8stesting.DeleteAction)
				if deletion.GetDeleteOptions().Preconditions == nil || *deletion.GetDeleteOptions().Preconditions.UID != "fixture-uid" {
					t.Fatal("cleanup is not bound to the created namespace UID")
				}
				if mode == "cleanup forbidden" {
					return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, namespace, errors.New("denied"))
				}
				return false, nil, nil
			})
			model := &policyTrafficModel{kube: kube, mode: "egress-only", denial: "timeout"}
			if mode == "absent" {
				model.mode = "absent"
			}
			deps := Deps{KubeClient: kube, ContextName: "chosen", RunCommand: func(ctx context.Context, input []byte, name string, args ...string) ([]byte, error) {
				if name != "kubectl" || !strings.Contains(strings.Join(args, " "), "--context chosen --namespace "+namespace+" exec ") {
					t.Fatal(args)
				}
				if mode == "exec forbidden" {
					return nil, errors.New("private diagnostic")
				}
				return model.run(ctx, input, name, args...)
			}}
			result := probeNetworkPolicy(ctx, Config{}, deps, policyProbeTiming{time.Second, 8 * time.Millisecond, 100 * time.Millisecond, 50 * time.Millisecond, time.Millisecond})
			want := "unverified"
			if mode == "enforced" {
				want = "enforced"
			}
			if result.Status != want {
				t.Fatalf("%+v, want %s", result, want)
			}
			if strings.Contains(result.Remedy, "private diagnostic") {
				t.Fatal("private diagnostic leaked")
			}
			if mode != "namespace forbidden" && mode != "cancelled" && mode != "create response lost" && !cleanupSeen {
				t.Fatal("namespace cleanup was not attempted")
			}
			if mode == "cancelled" || mode == "create response lost" {
				ns, err := kube.CoreV1().Namespaces().Get(t.Context(), namespace, metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				if cleanupSeen || !strings.Contains(result.Remedy, "Cleanup is unverified") {
					t.Fatal("unknown create UID acquired deletion authority", result)
				}
				ns.Annotations[policyProbeExpiry] = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
				if _, err = kube.CoreV1().Namespaces().Update(t.Context(), ns, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
				if _, err = reconcilePolicyProbes(t.Context(), deps, policyProbeTiming{interval: time.Millisecond}); err != nil {
					t.Fatal(err)
				}
			}
			_, err := kube.CoreV1().Namespaces().Get(t.Context(), namespace, metav1.GetOptions{})
			if mode != "cleanup forbidden" && !apierrors.IsNotFound(err) {
				t.Fatalf("scratch namespace remains: %v", err)
			}
			if mode == "enforced" && (createdPods != 3 || !strings.Contains(strings.Join(model.stages, ","), "timeout")) {
				t.Fatal(model.stages)
			}
			for _, action := range kube.Actions() {
				if action.GetResource().Resource == "daemonsets" {
					t.Fatal("controller-name detection returned")
				}
			}
		})
	}
}
