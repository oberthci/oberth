package argojob

import (
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestWorkflowTaskResultPolicyDeleteUsesOldObject(t *testing.T) {
	policy := renderNativeAdmissionProbe(t, "argo-task-result-policy.yaml", "-argo-task-results")
	found := false
	for _, rule := range policy.policy.Spec.MatchConstraints.ResourceRules {
		for _, op := range rule.Operations {
			if op == "DELETE" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("DELETE is outside task result policy")
	}
	c := &Controller{config: Config{ExecutorServiceAccount: "executor"}}
	wf := &wfv1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: "run-a", Namespace: "oberth-argo", UID: "run-a-uid"}}
	for _, account := range []string{"executor", "capability", "per-step"} {
		t.Run(account, func(t *testing.T) {
			request := taskResultRequest(account, c.identityPodName(wf, account))
			request["operation"] = "DELETE"
			own := taskResultObject(wf, wf.Name+"-123")
			if !policy.accepts(t, request, nil, own) {
				t.Fatal("own task result delete refused")
			}
			for _, mutation := range []string{"uid", "name", "label", "node", "owner", "unbound"} {
				t.Run(mutation, func(t *testing.T) {
					old := own.DeepCopy()
					r := taskResultRequest(account, c.identityPodName(wf, account))
					r["operation"] = "DELETE"
					switch mutation {
					case "uid":
						old.OwnerReferences[0].UID = "foreign-uid"
					case "name":
						old.OwnerReferences[0].Name = "foreign"
					case "label":
						old.Labels["workflows.argoproj.io/workflow"] = "foreign"
					case "node":
						old.Name = "foreign-123"
					case "owner":
						old.OwnerReferences = nil
					case "unbound":
						delete(r["userInfo"].(map[string]any), "extra")
					}
					if policy.accepts(t, r, nil, old) {
						t.Fatal("foreign task result delete admitted")
					}
				})
			}
		})
	}
}
