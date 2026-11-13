package installer

import (
	"context"
	"strings"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	kt "k8s.io/client-go/testing"
)

func TestPolicyProbeDeletesOnlyEachCreatedPolicyUID(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(map[bool]string{false: "all rounds", true: "replacement before delete"}[replace], func(t *testing.T) {
			kube := fake.NewClientset(preflightNode())
			probeNamespaceUIDFixture(kube)
			kube.PrependReactor("create", "pods", readyPolicyModelPod)
			deleted := map[string]bool{}
			attempts := 0
			var namespace string
			kube.PrependReactor("delete", "networkpolicies", func(action kt.Action) (bool, runtime.Object, error) {
				attempts++
				namespace = action.GetNamespace()
				deletion := action.(kt.DeleteAction)
				resource := networkingv1.SchemeGroupVersion.WithResource("networkpolicies")
				object, err := kube.Tracker().Get(resource, namespace, deletion.GetName())
				if err != nil {
					return true, nil, err
				}
				policy := object.(*networkingv1.NetworkPolicy).DeepCopy()
				expected := policy.UID
				options := deletion.GetDeleteOptions()
				if options.Preconditions == nil || options.Preconditions.UID == nil || *options.Preconditions.UID != expected || expected == "" {
					t.Error("delete lacks created policy UID", options)
					return true, nil, apierrors.NewBadRequest("missing UID")
				}
				if replace {
					policy.UID = "replacement-policy"
					if err := kube.Tracker().Delete(resource, namespace, policy.Name); err != nil {
						t.Fatal(err)
					}
					if err := kube.Tracker().Create(resource, policy, namespace); err != nil {
						t.Fatal(err)
					}
				}
				// Model the API server's atomic UID check, not fake.Clientset's name delete.
				if *options.Preconditions.UID != policy.UID {
					return true, nil, apierrors.NewConflict(schema.GroupResource{Group: networkingv1.GroupName, Resource: "networkpolicies"}, policy.Name, nil)
				}
				if deleted[string(expected)] {
					t.Error("policy UID reused across rounds")
				}
				deleted[string(expected)] = true
				return true, nil, kube.Tracker().Delete(resource, namespace, policy.Name)
			})
			model := &policyTrafficModel{kube: kube, mode: "egress-only", denial: "reset"}
			result := probeNetworkPolicy(t.Context(), Config{}, Deps{KubeClient: kube, RunCommand: model.run}, policyModelTiming())
			if replace {
				if result.Status != "unverified" || attempts != 1 || len(deleted) != 0 || !strings.Contains(result.Remedy, "egress policy removal") {
					t.Fatal("replacement deleted or measurement continued", result, attempts, deleted)
				}
				replacement, err := kube.NetworkingV1().NetworkPolicies(namespace).Get(t.Context(), "default-deny", metav1.GetOptions{})
				if err != nil || replacement.UID != "replacement-policy" {
					t.Fatal("per-policy delete removed replacement", err)
				}
			} else if result.Status != "enforced" || len(deleted) != policyProbeRounds {
				t.Fatal("round UID deletion not exercised", result, deleted)
			}
		})
	}
}

func TestPolicyProbeRequiresCreatedPolicyUID(t *testing.T) {
	kube := fake.NewClientset(preflightNode())
	probeNamespaceUIDFixture(kube)
	kube.PrependReactor("create", "pods", readyPolicyModelPod)
	kube.PrependReactor("create", "networkpolicies", func(a kt.Action) (bool, runtime.Object, error) { return true, a.(kt.CreateAction).GetObject(), nil })
	kube.PrependReactor("delete", "networkpolicies", func(kt.Action) (bool, runtime.Object, error) {
		t.Error("delete attempted without created UID")
		return true, nil, nil
	})
	model := &policyTrafficModel{kube: kube, mode: "egress-only", denial: "reset"}
	result := probeNetworkPolicy(t.Context(), Config{}, Deps{KubeClient: kube, RunCommand: model.run}, policyModelTiming())
	if result.Status != "unverified" || !strings.Contains(result.Remedy, "NetworkPolicy has no verified UID") {
		t.Fatal(result)
	}
}

func TestPolicyProbeDoesNotAdoptReplacementPolicyUID(t *testing.T) {
	kube := fake.NewClientset(preflightNode())
	probeNamespaceUIDFixture(kube)
	kube.PrependReactor("create", "pods", readyPolicyModelPod)
	model := &policyTrafficModel{kube: kube, mode: "egress-only", denial: "refused"}
	var createdUID string
	replaced, checked := false, false
	deps := Deps{KubeClient: kube, RunCommand: func(ctx context.Context, input []byte, command string, args ...string) ([]byte, error) {
		namespace, _, script, _ := policyModelArgs(args)
		if script == policyTrafficScript && !replaced {
			policies, err := kube.NetworkingV1().NetworkPolicies(namespace).List(ctx, metav1.ListOptions{})
			if err != nil {
				return nil, err
			}
			if len(policies.Items) > 0 {
				object := policies.Items[0].DeepCopy()
				createdUID = string(object.UID)
				object.UID = "replacement-before-delete"
				resource := networkingv1.SchemeGroupVersion.WithResource("networkpolicies")
				if err := kube.Tracker().Delete(resource, namespace, object.Name); err != nil {
					return nil, err
				}
				if err := kube.Tracker().Create(resource, object, namespace); err != nil {
					return nil, err
				}
				replaced = true
			}
		}
		return model.run(ctx, input, command, args...)
	}}
	kube.PrependReactor("delete", "networkpolicies", func(action kt.Action) (bool, runtime.Object, error) {
		checked = true
		deletion := action.(kt.DeleteAction)
		conditions := deletion.GetDeleteOptions().Preconditions
		if createdUID == "" || conditions == nil || conditions.UID == nil || string(*conditions.UID) != createdUID {
			t.Error("delete adopted a replacement instead of the creation UID", conditions)
		}
		return true, nil, apierrors.NewConflict(schema.GroupResource{Group: networkingv1.GroupName, Resource: "networkpolicies"}, deletion.GetName(), nil)
	})
	result := probeNetworkPolicy(t.Context(), Config{}, deps, policyModelTiming())
	if !replaced || !checked || result.Status != "unverified" || !strings.Contains(result.Remedy, "egress policy removal") {
		t.Fatal("replaced policy authorized removal or verification", result)
	}
}
