package argojob

import (
	"encoding/json"
	"errors"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

func TestVerifiedWorkflowSubmissionUsesSemanticResources(t *testing.T) {
	config := testConfig()
	config.NonrootProfile = argoworkflow.NonrootStaticProfile
	request := nonrootFixtureProof(testRequest(periapsis.TriggerCI, nonrootTestDocument))
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
	observed.Spec.ServiceAccountName = "changed-policy"
	err = sameSubmission(&observed, intended)
	var pending interface{ AmbiguousCreate() bool }
	var mismatch interface{ WorkflowSpecMismatch() bool }
	if err == nil || !errors.As(err, &pending) || !pending.AmbiguousCreate() || !errors.As(err, &mismatch) || !mismatch.WorkflowSpecMismatch() {
		t.Fatalf("owned changed spec must retain cleanup and admission: %v", err)
	}
	observed.Annotations[runIDAnnotation] = "foreign-run"
	err = sameSubmission(&observed, intended)
	if err == nil || (errors.As(err, &pending) && pending.AmbiguousCreate()) {
		t.Fatalf("foreign Workflow must never enter owned cleanup: %v", err)
	}
}
