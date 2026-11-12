package argojob

import (
	"encoding/json"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func TestNativeOnErrorRetriesReachWorkflowCreateUnchanged(t *testing.T) {
	wf := nativeWorkflow(t)
	retry := &wfv1.RetryStrategy{Limit: ptrIntString("2"), RetryPolicy: wfv1.RetryPolicyOnError, Backoff: &wfv1.Backoff{Duration: "30s", Factor: ptrIntString("2")}}
	wf.Spec.Templates[0].RetryStrategy = retry
	actual := nativeSubmit(t, wf)
	before, _ := json.Marshal(retry)
	after, _ := json.Marshal(actual.Spec.Templates[0].RetryStrategy)
	if string(before) != string(after) {
		t.Fatalf("retry changed: %s -> %s", before, after)
	}
}
func ptrIntString(value string) *intstr.IntOrString { v := intstr.FromString(value); return &v }
