package argojob

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func bindCapabilityLoader(t *testing.T, controller *Controller, load func(context.Context, int64) (*argoworkflow.Capability, error)) {
	t.Helper()
	setter, ok := any(controller).(interface {
		WithWorkflowCapabilityLoader(func(context.Context, int64) (*argoworkflow.Capability, error)) *Controller
	})
	if !ok {
		t.Fatal("controller has no live capability loader")
	}
	setter.WithWorkflowCapabilityLoader(load)
}

func operatorKubeClient(grant *argoworkflow.Capability) *fake.Clientset {
	kube := fake.NewClientset(&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: grant.ServiceAccount, Namespace: grant.Namespace}})
	kube.Resources = []*metav1.APIResourceList{{GroupVersion: "tasks.example.test/v1", APIResources: []metav1.APIResource{{Name: "tasks", Kind: "Task", Namespaced: true}}}}
	return kube
}

func TestCapabilityAdoptionRechecksGrantRepositoryAndNamespaceScope(t *testing.T) {
	grant := operatorCapability()
	submission := requestWithCapability(t, operatorResourceDocument, grant)
	wf, err := Build(testConfig(), submission)
	if err != nil {
		t.Fatal(err)
	}
	wf.UID = "operator-workflow-uid"
	for _, scenario := range []string{"valid", "revoked", "changed", "foreign repository", "missing account", "cluster scoped", "missing discovery", "read failure", "old operator identity", "step token", "resource executor"} {
		t.Run(scenario, func(t *testing.T) {
			kube := operatorKubeClient(grant)
			current := operatorCapability()
			existing := wf.DeepCopy()
			repoID := submission.RepositoryID
			switch scenario {
			case "old operator identity":
				existing.Spec.ServiceAccountName = grant.ServiceAccount
			case "step token":
				*existing.Spec.Templates[0].AutomountServiceAccountToken = true
			case "resource executor":
				existing.Spec.Templates[0].Executor.ServiceAccountName = testExecutorAcct
			case "revoked":
				current = nil
			case "changed":
				current.ResourceKinds[0].Actions = append(current.ResourceKinds[0].Actions, "delete")
			case "foreign repository":
				repoID++
			case "missing account":
				if err := kube.CoreV1().ServiceAccounts(grant.Namespace).Delete(t.Context(), grant.ServiceAccount, metav1.DeleteOptions{}); err != nil {
					t.Fatal(err)
				}
			case "cluster scoped":
				kube.Resources[0].APIResources[0].Namespaced = false
			case "missing discovery":
				kube.Resources = nil
			}
			controller := &Controller{kube: kube, workflows: &staticWorkflowClient{workflow: existing}, config: testConfig()}
			bindCapabilityLoader(t, controller, func(_ context.Context, id int64) (*argoworkflow.Capability, error) {
				if id != repoID {
					t.Fatalf("read grant for %d, want durable repository %d", id, repoID)
				}
				if scenario == "read failure" {
					return nil, errors.New("store unavailable")
				}
				return current, nil
			})
			r := AdoptionRequest{Name: submission.Name, RunID: submission.RunID, Ref: submission.Ref, SHA: submission.SHA, Trigger: submission.Trigger, RunTrigger: argoworkflow.RunTriggerBranch}
			body, _ := json.Marshal(map[string]any{"RepositoryID": repoID})
			if err := json.Unmarshal(body, &r); err != nil {
				t.Fatal(err)
			}
			binding, found, err := controller.Adopt(t.Context(), r)
			if scenario == "valid" {
				if err != nil || !found || binding.UID != wf.UID {
					t.Fatalf("adoption: %+v found=%v err=%v", binding, found, err)
				}
				changed := wf.DeepCopy()
				delete(changed.Annotations, capabilityAnnotation)
				if binding.matches(changed) {
					t.Fatal("capability binding was not sealed into recovery")
				}
			} else if err == nil || found {
				t.Fatalf("unsafe adoption accepted: found=%v err=%v", found, err)
			}
		})
	}
}

func TestCapabilityCreateRefusesRevokedGrantBeforeSourceSeeding(t *testing.T) {
	r := requestWithCapability(t, operatorResourceDocument, operatorCapability())
	c := &Controller{config: testConfig(), kube: operatorKubeClient(operatorCapability())}
	bindCapabilityLoader(t, c, func(context.Context, int64) (*argoworkflow.Capability, error) { return nil, nil })
	if _, err := c.Create(t.Context(), r); err == nil || !strings.Contains(err.Error(), "capability changed") {
		t.Fatalf("revocation must be refused before seeding: %v", err)
	}
}
