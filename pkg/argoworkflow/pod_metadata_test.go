package argoworkflow

import (
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
)

func TestAuthorPodMetadataAdmitted(t *testing.T) {
	wf := decodeAdmissible(t, nil)
	wf.Spec.PodMetadata = &wfv1.Metadata{Labels: map[string]string{"app": "store"}, Annotations: map[string]string{"example.test/owner": "team"}}
	wf.Spec.Templates[0].Metadata = wfv1.Metadata{Labels: map[string]string{"security": "S2"}, Annotations: map[string]string{"example.test/note": "{{workflow.name}}"}}
	if err := Admit(wf, Policy{}); err != nil {
		t.Fatal(err)
	}
}

func TestReservedPodMetadataKeysRefusedAtEveryScope(t *testing.T) {
	keys := []string{"oberth.ci/run", "workflows.argoproj.io/workflow", "workflows.argoproj.io/unknown", "app.kubernetes.io/name", "app.kubernetes.io/instance", "app.kubernetes.io/component", "component", "openbao-active", "openbao-internal", "kubernetes.io/metadata.name", "kubectl.kubernetes.io/default-container", "{{workflow.parameters.key}}"}
	for _, scope := range []string{"pod", "template", "defaults", "inline"} {
		for _, field := range []string{"labels", "annotations"} {
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
