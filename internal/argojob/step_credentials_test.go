package argojob

import (
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func TestAdmissionCredentialEvidenceIncludesTemplatesBeyondRecursiveBackEdges(t *testing.T) {
	workflow := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Entrypoint: "loop", Templates: []wfv1.Template{
		{Name: "loop", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "again", Template: "loop"}, {Name: "check", Template: "secret"}}}},
		{Name: "plain", Container: &corev1.Container{}},
		{Name: "secret", Container: &corev1.Container{}},
		{Name: "projected", Volumes: []corev1.Volume{{Name: "token", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Audience: "openbao", Path: "token"}}}}}}}},
	}}}
	request := Request{StepGrants: map[string]map[string]bool{"approved-path": {"secret": true}}}
	evidence := StepCredentialEvidence(workflow, request)
	if len(evidence) != 4 {
		t.Fatal("recursive templates were cut", evidence)
	}
	byName := map[string]bool{}
	for _, step := range evidence {
		if step.Complete {
			t.Fatal("cut recursive graph granted replay authority", step)
		}
		byName[step.Template] = step.Credentialed
	}
	if byName["plain"] || !byName["secret"] || !byName["projected"] {
		t.Fatal("credential scope depends on the display plan", evidence)
	}
}

func TestAdmissionEvidenceKeepsNonPodAndCapabilityReplayGated(t *testing.T) {
	workflow := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Entrypoint: "plain", Templates: []wfv1.Template{
		{Name: "plain", Container: &corev1.Container{}},
		{Name: "resource", Resource: &wfv1.ResourceTemplate{}},
		{Name: "http", HTTP: &wfv1.HTTP{}},
	}}}
	evidence := StepCredentialEvidence(workflow, Request{})
	if !evidence[0].Known || evidence[0].Credentialed || evidence[1].Known || evidence[2].Known {
		t.Fatal("unclassified native execution became credential-free", evidence)
	}
	evidence = StepCredentialEvidence(workflow, Request{Capability: &argoworkflow.Capability{Namespace: "pipelines", ServiceAccount: "operator-capability"}})
	if !evidence[1].Known || !evidence[1].Credentialed || len(evidence[1].Grants) != 1 {
		t.Fatal("resource capability lost replay gate", evidence)
	}
}
