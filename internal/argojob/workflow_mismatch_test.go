package argojob

import (
	"encoding/json"
	"errors"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/oberthci/oberth/pkg/pipeline"
)

func TestVerifiedWorkflowSubmissionUsesSemanticResources(t *testing.T) {
	config := testConfig()

	request := testRequest(pipeline.TriggerCI, authoredNonrootDocument)
	request.SourceVolume = SourceVolume{ClaimName: "run-source", SubPath: "src", ArtifactsSubPath: "artifacts"}
	intended, err := Build(config, request)
	if err != nil {
		t.Fatal(err)
	}
	for i := range intended.Spec.Templates {
		if c := intended.Spec.Templates[i].Container; c != nil {
			if c.Resources.Requests == nil {
				c.Resources.Requests = corev1.ResourceList{}
			}
			c.Resources.Requests[corev1.ResourceMemory] = resource.MustParse("1Gi")
		}
	}
	body, err := json.Marshal(intended)
	if err != nil {
		t.Fatal(err)
	}
	var observed wfv1.Workflow
	if err := json.Unmarshal(body, &observed); err != nil {
		t.Fatal(err)
	}
	for i := range observed.Spec.Templates {
		if c := observed.Spec.Templates[i].Container; c != nil {
			c.Resources.Requests[corev1.ResourceMemory] = resource.MustParse("1024Mi")
			for name, quantity := range c.Resources.Requests {
				quantity.AsDec()
				c.Resources.Requests[name] = quantity
			}
		}
	}
	if err := sameSubmission(&observed, intended); err != nil {
		t.Fatalf("Kubernetes serialization changed no resource value: %v", err)
	}
	// Recovery uses submission identity and pinned source checks for every workflow.
	// The removed annotation no longer enables a full-spec comparison.
	observed.Spec.ServiceAccountName = "changed-policy"
	if err := sameSubmission(&observed, intended); err != nil {
		t.Fatalf("non-identity spec field changed the recovery contract: %v", err)
	}
	var pending interface{ AmbiguousCreate() bool }
	observed.Annotations[runIDAnnotation] = "foreign-run"
	err = sameSubmission(&observed, intended)
	if err == nil || (errors.As(err, &pending) && pending.AmbiguousCreate()) {
		t.Fatalf("foreign Workflow must never enter owned cleanup: %v", err)
	}
}
