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

// Argo's controller deletes a fulfilled Workflow's results with DeleteCollection
// (one DELETE per object) under its own Pod-bound token, which is no holder.
func TestWorkflowTaskResultPolicyAdmitsWorkflowWriterCleanup(t *testing.T) {
	policy := renderNativeAdmissionProbe(t, "argo-task-result-policy.yaml", "-argo-task-results")
	exempt := false
	for _, condition := range policy.policy.Spec.MatchConditions {
		if condition.Expression == "request.operation != 'DELETE' || !authorizer.group('argoproj.io').resource('workflows').namespace(request.namespace).check('update').allowed()" {
			exempt = true
		}
	}
	if !exempt {
		t.Fatal("DELETE exemption must ask the authorizer for update on Workflows in the result's namespace")
	}
	wf := &wfv1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: "run-a", Namespace: "oberth-argo", UID: "run-a-uid"}}
	result := taskResultObject(wf, wf.Name+"-123")
	controller := taskResultRequest("argo", "workflow-controller-5d8f7c9b6-abcde")
	controller["operation"] = "DELETE"
	if !policy.acceptsAuthorized(t, controller, nil, result, true) {
		t.Fatal("Workflow writer's task result cleanup refused")
	}
	if policy.acceptsAuthorized(t, controller, nil, result, false) {
		t.Fatal("unbound requester without Workflow update deleted a task result")
	}
	for _, operation := range []string{"CREATE", "UPDATE"} {
		write := taskResultRequest("argo", "workflow-controller-5d8f7c9b6-abcde")
		write["operation"] = operation
		if policy.acceptsAuthorized(t, write, result, result, true) {
			t.Fatalf("Workflow writer exemption reached %s", operation)
		}
	}
	c := &Controller{config: Config{ExecutorServiceAccount: "executor"}}
	foreign := taskResultObject(&wfv1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: "run-b", Namespace: "oberth-argo", UID: "run-b-uid"}}, "run-b-123")
	for _, account := range []string{"executor", "capability"} {
		request := taskResultRequest(account, c.identityPodName(wf, account))
		request["operation"] = "DELETE"
		if policy.acceptsAuthorized(t, request, nil, foreign, false) {
			t.Fatalf("%s holder without Workflow update deleted a foreign result", account)
		}
		if !policy.acceptsAuthorized(t, request, nil, result, false) {
			t.Fatalf("%s holder could not delete its own result", account)
		}
	}
}
