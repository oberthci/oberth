package argojob

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/ext"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ktypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/yaml"
)

type nativeAdmissionProbe struct {
	policy                        admissionv1.ValidatingAdmissionPolicy
	conditions, variables, checks []cel.Program
}

func renderNativeAdmissionProbe(t *testing.T, file, suffix string) nativeAdmissionProbe {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "helm", "template", "oberth", "../../charts/oberth", "--set", "image.ref=example.invalid/oberth@sha256:"+strings.Repeat("1", 64), "--show-only", "templates/"+file)
	body, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("render native policy: %v: %s", err, body)
	}
	var p nativeAdmissionProbe
	found := false
	for _, document := range strings.Split(string(body), "\n---") {
		var candidate admissionv1.ValidatingAdmissionPolicy
		if err = yaml.Unmarshal([]byte(document), &candidate); err != nil {
			t.Fatal(err)
		}
		if candidate.Kind == "ValidatingAdmissionPolicy" && strings.HasSuffix(candidate.Name, suffix) {
			p.policy = candidate
			found = true
			break
		}
	}
	if !found {
		t.Fatal("rendered policy missing")
	}
	if p.policy.Spec.FailurePolicy == nil || *p.policy.Spec.FailurePolicy != admissionv1.Fail {
		t.Fatal("policy must fail closed")
	}
	env, err := cel.NewEnv(cel.Variable("object", cel.DynType), cel.Variable("oldObject", cel.DynType), cel.Variable("request", cel.DynType), cel.Variable("variables", cel.DynType), ext.Strings())
	if err != nil {
		t.Fatal(err)
	}
	compile := func(expression string) cel.Program {
		t.Helper()
		ast, issues := env.Compile(expression)
		if issues.Err() != nil {
			t.Fatal(issues.Err())
		}
		program, err := env.Program(ast)
		if err != nil {
			t.Fatal(err)
		}
		return program
	}
	for _, v := range p.policy.Spec.MatchConditions {
		p.conditions = append(p.conditions, compile(v.Expression))
	}
	for _, v := range p.policy.Spec.Variables {
		p.variables = append(p.variables, compile(v.Expression))
	}
	for _, v := range p.policy.Spec.Validations {
		p.checks = append(p.checks, compile(v.Expression))
	}
	return p
}

func (p nativeAdmissionProbe) accepts(t *testing.T, request map[string]any, object, old any) bool {
	t.Helper()
	jsonValue := func(value any) any {
		body, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var result any
		if err = json.Unmarshal(body, &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	variables := map[string]any{}
	activation := map[string]any{"request": request, "object": jsonValue(object), "oldObject": jsonValue(old), "variables": variables}
	for _, program := range p.conditions {
		value, _, err := program.Eval(activation)
		if err != nil {
			return false
		}
		if value != types.True {
			return true
		}
	}
	for i, program := range p.variables {
		value, _, err := program.Eval(activation)
		if err != nil {
			return false
		}
		variables[p.policy.Spec.Variables[i].Name] = value
	}
	for _, program := range p.checks {
		value, _, err := program.Eval(activation)
		if err != nil || value != types.True {
			return false
		}
	}
	return true
}

func taskResultRequest(account, pod string) map[string]any {
	return map[string]any{"namespace": "oberth-argo", "operation": "CREATE", "userInfo": map[string]any{"username": "system:serviceaccount:oberth-argo:" + account, "extra": map[string]any{"authentication.kubernetes.io/pod-name": []string{pod}, "authentication.kubernetes.io/pod-uid": []string{"holder-uid"}}}}
}

func taskResultObject(workflow *wfv1.Workflow, node string) *wfv1.WorkflowTaskResult {
	return &wfv1.WorkflowTaskResult{ObjectMeta: metav1.ObjectMeta{Name: node, Namespace: workflow.Namespace, Labels: map[string]string{"workflows.argoproj.io/workflow": workflow.Name}, OwnerReferences: []metav1.OwnerReference{{APIVersion: "argoproj.io/v1alpha1", Kind: "Workflow", Name: workflow.Name, UID: workflow.UID}}}}
}

func TestWorkflowTaskResultPolicyScopesBoundIdentity(t *testing.T) {
	policy := renderNativeAdmissionProbe(t, "argo-task-result-policy.yaml", "-argo-task-results")
	controller := &Controller{config: Config{ExecutorServiceAccount: "executor"}}
	workflow := &wfv1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: "run-a", Namespace: "oberth-argo", UID: "11111111-1111-1111-1111-111111111111"}}
	mutations := map[string]func(map[string]any, *wfv1.WorkflowTaskResult){
		"foreign-name": func(_ map[string]any, o *wfv1.WorkflowTaskResult) { o.Name = "run-b-123" },
		"foreign-label": func(_ map[string]any, o *wfv1.WorkflowTaskResult) {
			o.Labels["workflows.argoproj.io/workflow"] = "run-b"
		},
		"foreign-owner-uid": func(_ map[string]any, o *wfv1.WorkflowTaskResult) { o.OwnerReferences[0].UID = "other-uid" },
		"foreign-owner-name": func(_ map[string]any, o *wfv1.WorkflowTaskResult) {
			o.OwnerReferences[0].Name = "run-b"
			o.Labels["workflows.argoproj.io/workflow"] = "run-b"
			o.Name = "run-b-123"
		},
		"owner-kind":    func(_ map[string]any, o *wfv1.WorkflowTaskResult) { o.OwnerReferences[0].Kind = "Pod" },
		"owner-api":     func(_ map[string]any, o *wfv1.WorkflowTaskResult) { o.OwnerReferences[0].APIVersion = "v1" },
		"missing-owner": func(_ map[string]any, o *wfv1.WorkflowTaskResult) { o.OwnerReferences = nil },
		"multiple-owners": func(_ map[string]any, o *wfv1.WorkflowTaskResult) {
			o.OwnerReferences = append(o.OwnerReferences, o.OwnerReferences[0])
		},
		"missing-label":   func(_ map[string]any, o *wfv1.WorkflowTaskResult) { o.Labels = nil },
		"non-node-suffix": func(_ map[string]any, o *wfv1.WorkflowTaskResult) { o.Name = workflow.Name + "-other-node" },
		"unbound-token":   func(r map[string]any, _ *wfv1.WorkflowTaskResult) { delete(r["userInfo"].(map[string]any), "extra") },
		"old-holder-name": func(r map[string]any, _ *wfv1.WorkflowTaskResult) {
			r["userInfo"].(map[string]any)["extra"].(map[string]any)["authentication.kubernetes.io/pod-name"] = []string{"oberth-executor-oldhash"}
		},
		"missing-pod-uid": func(r map[string]any, _ *wfv1.WorkflowTaskResult) {
			delete(r["userInfo"].(map[string]any)["extra"].(map[string]any), "authentication.kubernetes.io/pod-uid")
		},
		"duplicate-claim": func(r map[string]any, _ *wfv1.WorkflowTaskResult) {
			r["userInfo"].(map[string]any)["extra"].(map[string]any)["authentication.kubernetes.io/pod-name"] = []string{"first", "second"}
		},
	}
	for _, account := range []string{"executor", "operator-capability", "per-step-account"} {
		t.Run(account, func(t *testing.T) {
			holder := controller.identityPodName(workflow, account)
			for _, node := range []string{workflow.Name, workflow.Name + "-123"} {
				if !policy.accepts(t, taskResultRequest(account, holder), taskResultObject(workflow, node), nil) {
					t.Fatal("same-run positive refused")
				}
			}
			for name, mutate := range mutations {
				t.Run(name, func(t *testing.T) {
					r := taskResultRequest(account, holder)
					o := taskResultObject(workflow, workflow.Name+"-123")
					mutate(r, o)
					if policy.accepts(t, r, o, nil) {
						t.Fatal("foreign or unbound task result admitted")
					}
				})
			}
			t.Run("old-object-takeover", func(t *testing.T) {
				r := taskResultRequest(account, holder)
				r["operation"] = "UPDATE"
				o := taskResultObject(workflow, workflow.Name+"-123")
				old := o.DeepCopy()
				old.OwnerReferences[0].UID = "other-uid"
				if policy.accepts(t, r, o, old) {
					t.Fatal("existing foreign task result reassigned")
				}
			})
			r := taskResultRequest(account, holder)
			r["operation"] = "UPDATE"
			o := taskResultObject(workflow, workflow.Name+"-123")
			if !policy.accepts(t, r, o, o.DeepCopy()) {
				t.Fatal("same-run patch refused")
			}
		})
	}
}

func TestExecutorIdentityPodNamesAreReserved(t *testing.T) {
	policy := renderNativeAdmissionProbe(t, "argo-task-result-policy.yaml", "-argo-executor-holders")
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "oberth-executor.uid.executor.run"}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "identity"}}}}
	for _, operation := range []string{"CREATE", "UPDATE"} {
		for _, account := range []string{"executor", "operator-capability", "per-step-account"} {
			t.Run(operation+"/"+account, func(t *testing.T) {
				r := taskResultRequest(account, "")
				r["operation"] = operation
				if policy.accepts(t, r, pod, pod) {
					t.Fatal("pipeline identity can forge or alter an identity holder")
				}
				copy := pod.DeepCopy()
				copy.Name = ""
				copy.GenerateName = "oberth-executor."
				if policy.accepts(t, r, copy, nil) {
					t.Fatal("pipeline identity can generate a reserved holder name")
				}
			})
		}
	}
	server := taskResultRequest("server", "")
	server["userInfo"].(map[string]any)["username"] = "system:serviceaccount:oberth:server"
	if !policy.accepts(t, server, pod, nil) {
		t.Fatal("trusted server holder refused")
	}
}

func TestExecutorIdentityHolderNameBinding(t *testing.T) {
	c := &Controller{config: Config{ExecutorServiceAccount: "executor"}}
	wf := &wfv1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: strings.Repeat("a", 63), UID: ktypes.UID("11111111-1111-1111-1111-111111111111")}}
	first := c.identityPodName(wf, "executor")
	second := c.identityPodName(wf, "operator")
	if first == second || len(second) > 253 || !strings.Contains(first, "."+string(wf.UID)+".") || !strings.HasSuffix(first, "."+wf.Name) {
		t.Fatal("holder name lost its authenticated Workflow binding")
	}
	if c.identityPodName(wf, "operator") != second {
		t.Fatal("unstable holder name")
	}
}

func TestWorkflowTaskResultPolicyRejectsAmbiguousHolderSegments(t *testing.T) {
	policy := renderNativeAdmissionProbe(t, "argo-task-result-policy.yaml", "-argo-task-results")
	controller := &Controller{config: Config{ExecutorServiceAccount: "executor"}}
	workflow := &wfv1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: "executor.run", Namespace: "oberth-argo", UID: "11111111-1111-1111-1111-111111111111"}}
	for _, account := range []string{"executor", "operator-capability", "per-step-account"} {
		t.Run(account, func(t *testing.T) {
			request := taskResultRequest(account, controller.identityPodName(workflow, account))
			if !policy.accepts(t, request, taskResultObject(workflow, workflow.Name), nil) {
				t.Fatal("dotted Workflow name refused")
			}
			forged := taskResultObject(workflow, "run")
			forged.OwnerReferences[0].Name = "run"
			forged.Labels["workflows.argoproj.io/workflow"] = "run"
			role := strings.Split(controller.identityPodName(workflow, account), ".")[2]
			forged.OwnerReferences[0].UID = ktypes.UID(string(workflow.UID) + "." + role)
			if policy.accepts(t, request, forged, nil) {
				t.Fatal("owner UID consumed the holder role and reassigned a dotted Workflow")
			}
		})
	}
}
