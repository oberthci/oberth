package argojob_test

import (
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	argoutil "github.com/argoproj/argo-workflows/v4/workflow/util"
	"github.com/oberthci/oberth/internal/argojob/argojobtest"
	admissionv1 "k8s.io/api/admission/v1"
	auth "k8s.io/api/authentication/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/apiserver/pkg/authentication/user"
)

func TestTaskResultTokenIsConfinedToItsOwnPod(t *testing.T) {
	t.Parallel()
	workflow := &wf.Workflow{ObjectMeta: meta.ObjectMeta{Name: "run-0001"}}
	for _, version := range []argoutil.PodNameVersion{argoutil.PodNameV1, argoutil.PodNameV2} {
		for _, node := range []string{workflow.Name, workflow.Name + ".build(0)", workflow.Name + ".build(1)"} {
			pod := argoutil.GeneratePodName(workflow.Name, node, "build-with-dashes", workflow.NodeID(node), version)
			for _, operation := range []admission.Operation{admission.Create, admission.Update} {
				for _, tc := range []struct {
					name, namespace, workflow, result string
					want                              bool
				}{
					{"own", "ci", workflow.Name, workflow.NodeID(node), true},
					{"another Pod in own Workflow", "ci", workflow.Name, workflow.NodeID(workflow.Name + ".other"), false},
					{"another Workflow", "ci", "run-0002", "run-0002-123", false},
					{"another namespace", "other", workflow.Name, workflow.NodeID(node), false},
				} {
					t.Run(string(version)+"/"+node+"/"+string(operation)+"/"+tc.name, func(t *testing.T) {
						assertTaskResultScope(t, taskResultScope{tc.namespace, tc.workflow, tc.result, pod, operation, tc.want})
					})
				}
			}
		}
	}
}

type taskResultScope struct {
	namespace, workflow, result, pod string
	operation                        admission.Operation
	want                             bool
}

func assertTaskResultScope(t *testing.T, c taskResultScope) {
	t.Helper()
	policy := argojobtest.TaskResultPolicy("ci")
	check, match := policyEvaluators(t, policy)
	object := resultObject(c.workflow)
	object.SetName(c.result)
	object.SetNamespace(c.namespace)
	old := object.DeepCopy()
	extras := map[string][]string{"authentication.kubernetes.io/pod-name": {c.pod}}
	u := &user.DefaultInfo{Name: "system:serviceaccount:ci:pipeline", Extra: extras}
	attr := admission.NewAttributesRecord(object, old, schema.GroupVersionKind{Group: "argoproj.io", Version: "v1alpha1", Kind: "WorkflowTaskResult"}, c.namespace, c.result, schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "workflowtaskresults"}, "", c.operation, nil, false, u)
	va := &admission.VersionedAttributes{Attributes: attr, VersionedKind: attr.GetKind(), VersionedObject: object, VersionedOldObject: old}
	req := &admissionv1.AdmissionRequest{Operation: admissionv1.Operation(c.operation), Namespace: c.namespace, UserInfo: auth.UserInfo{Username: u.Name, Extra: map[string]auth.ExtraValue{"authentication.kubernetes.io/pod-name": {c.pod}}}}
	if !evaluate(t, match, va, req) {
		t.Fatal("pipeline account escaped the guarded policy")
	}
	if got := evaluate(t, check, va, req); got != c.want {
		t.Fatalf("accepted=%v want %v", got, c.want)
	}
}

func TestTaskResultPolicyAllowsOnlySpecifiedPodPrefixes(t *testing.T) {
	t.Parallel()
	const pod = "run-0001-build-with-dashes-123"
	for _, operation := range []admission.Operation{admission.Create, admission.Update} {
		for _, tc := range []struct {
			label, name string
			want        bool
		}{
			{"run-0001", "run-0001-123", true},
			{"run", "run-123", true},
			{"run-0001-build", "run-0001-build-123", true},
			{"run-0001-build-with-dashes", pod, true},
			{pod, pod, true},
			{"run-0001-buil", "run-0001-buil-123", false},
			{"run-other", "run-other-123", false},
			{"run", "run-456", false},
		} {
			t.Run(string(operation)+"/"+tc.label+"/"+tc.name, func(t *testing.T) {
				assertTaskResultScope(t, taskResultScope{namespace: "ci", workflow: tc.label, result: tc.name, pod: pod, operation: operation, want: tc.want})
			})
		}
	}
}
