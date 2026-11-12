package argojob

import (
	"context"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func TestReviewCPUResourceOnlyWorkflowRemainsAdoptable(t *testing.T) {
	grant := operatorCapability()
	request := requestWithCapability(t, operatorResourceDocument, grant)
	request.RunTrigger = argoworkflow.RunTriggerBranch
	request.PipelineFile = ".oberth/build.yaml"
	resolved, err := argoworkflow.ResolveRunSource(request.Source, nil, argoworkflow.RunSourceBinding{RunID: request.RunID, RepositoryID: request.RepositoryID, SHA: request.SHA, File: request.PipelineFile, Trigger: request.RunTrigger})
	if err != nil {
		t.Fatal(err)
	}
	request.DemandReceipt, err = resolved.Receipt(argoworkflow.Policy{ServerDeadline: true, Capability: grant})
	if err != nil {
		t.Fatal(err)
	}
	c := testConfig()
	wf, err := Build(c, request)
	if err != nil {
		t.Fatal(err)
	}
	wf.UID = "review-resource-workflow"
	t.Logf("stamped demand: %s", wf.Annotations[podDemandAnnotation])
	controller := &Controller{kube: operatorKubeClient(grant), workflows: &staticWorkflowClient{workflow: wf}, config: c}
	bindCapabilityLoader(t, controller, func(context.Context, int64) (*argoworkflow.Capability, error) { return grant, nil })
	binding, found, err := controller.Adopt(t.Context(), AdoptionRequest{Name: request.Name, RunID: request.RunID, Ref: request.Ref, SHA: request.SHA, Trigger: request.Trigger, RunTrigger: request.RunTrigger, RepositoryID: request.RepositoryID})
	if err != nil || !found || !binding.Pods.Valid() {
		t.Fatalf("fresh CPU-sized valid resource workflow refused on restart: found=%v err=%v", found, err)
	}
	for _, raw := range []string{`{"cpu_milli":50,"memory_bytes":0,"pods":1}`, `invalid`} {
		wf.Annotations[podDemandAnnotation] = raw
		unknown, found, err := controller.Adopt(t.Context(), AdoptionRequest{Name: request.Name, RunID: request.RunID, Ref: request.Ref, SHA: request.SHA, Trigger: request.Trigger, RunTrigger: request.RunTrigger, RepositoryID: request.RepositoryID})
		if err != nil || !found || unknown.Pods.Valid() {
			t.Fatalf("invalid advisory binding blocked adoption: %+v %v", unknown, err)
		}
	}

}

func TestInvalidDerivedDemandIsNotPersisted(t *testing.T) {
	wf := &wfv1.Workflow{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{podDemandAnnotation: "old"}}}
	stampPodDemand(wf, &argoworkflow.PodDemand{CPUMilli: 50, Pods: 1})
	if wf.Annotations[podDemandAnnotation] != "" {
		t.Fatal("invalid derived binding written")
	}
}
