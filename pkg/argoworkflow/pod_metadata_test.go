package argoworkflow

import (
	"errors"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
)

func TestAuthorPodLabelsAdmitted(t *testing.T) {
	wf := decodeAdmissible(t, nil)
	wf.Spec.PodMetadata = &wfv1.Metadata{Labels: map[string]string{"app": "store"}}
	wf.Spec.Templates[0].Metadata = wfv1.Metadata{Labels: map[string]string{"security": "S2"}}
	if err := Admit(wf, Policy{}); err != nil {
		t.Fatal(err)
	}
}

func TestReservedPodMetadataKeysRefusedAtEveryScope(t *testing.T) {
	keys := []string{"oberth.ci/run", "workflows.argoproj.io/workflow", "workflows.argoproj.io/unknown", "app.kubernetes.io/name", "app.kubernetes.io/instance", "app.kubernetes.io/component", "component", "openbao-active", "openbao-internal", "kubernetes.io/metadata.name", "kubectl.kubernetes.io/default-container", "{{workflow.parameters.key}}"}
	for _, scope := range []string{"pod", "template", "defaults", "inline", "artifactGC"} {
		for _, field := range []string{"labels"} {
			for _, key := range keys {
				t.Run(scope+"/"+field+"/"+key, func(t *testing.T) {
					wf := decodeAdmissible(t, nil)
					metadata := wfv1.Metadata{}
					if field == "labels" {
						metadata.Labels = map[string]string{key: "other"}
					} else {
						metadata.Annotations = map[string]string{key: "other"}
					}
					switch scope {
					case "artifactGC":
						wf.Spec.ArtifactGC = &wfv1.WorkflowLevelArtifactGC{ArtifactGC: wfv1.ArtifactGC{PodMetadata: &metadata}}
					case "pod":
						wf.Spec.PodMetadata = &metadata
					case "template":
						wf.Spec.Templates[0].Metadata = metadata
					case "defaults":
						wf.Spec.TemplateDefaults = &wfv1.Template{Metadata: metadata}
					case "inline":
						leaf := wf.Spec.Templates[0]
						leaf.Metadata = metadata
						wf.Spec.Templates[0] = wfv1.Template{Name: leaf.Name, DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "leaf", Inline: &leaf}}}}
					}
					err := Admit(wf, Policy{})
					if err == nil || !strings.Contains(err.Error(), key) {
						t.Fatal("reserved/computed metadata admitted", err)
					}
				})
			}
		}
	}
}

func TestAuthorPodAnnotationBehaviorSwitchesRefused(t *testing.T) {
	for _, scope := range []string{"pod", "template", "defaults", "inline", "artifactGC"} {
		for _, key := range []string{"container.apparmor.security.beta.kubernetes.io/main", "k8s.v1.cni.cncf.io/networks", "cni.projectcalico.org/ipAddrs", "sidecar.istio.io/inject", "vault.hashicorp.com/agent-inject", "io.kubernetes.cri-o.Devices", "kubernetes.io/egress-bandwidth", "example.test/note"} {
			t.Run(scope+"/"+key, func(t *testing.T) {
				wf := decodeAdmissible(t, nil)
				metadata := wfv1.Metadata{Annotations: map[string]string{key: "unconfined"}}
				switch scope {
				case "artifactGC":
					wf.Spec.ArtifactGC = &wfv1.WorkflowLevelArtifactGC{ArtifactGC: wfv1.ArtifactGC{PodMetadata: &metadata}}
				case "pod":
					wf.Spec.PodMetadata = &metadata
				case "template":
					wf.Spec.Templates[0].Metadata = metadata
				case "defaults":
					wf.Spec.TemplateDefaults = &wfv1.Template{Metadata: metadata}
				case "inline":
					leaf := wf.Spec.Templates[0]
					leaf.Metadata = metadata
					wf.Spec.Templates[0] = wfv1.Template{Name: leaf.Name, DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "leaf", Inline: &leaf}}}}
				}
				if err := Admit(wf, Policy{}); err == nil || !strings.Contains(err.Error(), "annotations is assigned by Oberth") {
					t.Fatal("author Pod annotation admitted", err)
				}
			})
		}
	}
}

func TestInvalidOperatorPolicyIsNotADocumentRefusal(t *testing.T) {
	wf := decodeAdmissible(t, nil)
	err := Admit(wf, Policy{RunnerImagePrefixes: []string{"INVALID PREFIX"}})
	var refusal *AdmissionError
	if err == nil || errors.As(err, &refusal) {
		t.Fatal("operator policy blamed on document", err)
	}
}
