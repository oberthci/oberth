package argoworkflow

import (
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

func TestAdmitCapabilityResourceRefusesHelpersEverywhere(t *testing.T) {
	const refusal = "resource templates may not declare initContainers or sidecars"
	for _, carrier := range []string{"direct", "defaults", "containerSet", "inline-dag", "inline-steps", "workflow-hook", "task-hook", "step-hook"} {
		for _, helpers := range []string{"initContainers", "sidecars", "both"} {
			t.Run(carrier+"/"+helpers, func(t *testing.T) {
				wf := decodeAdmissible(t, nil)
				image := wf.Spec.Templates[0].Container.Image
				leaf := executorAdmissionLeaf(t, wf, carrier)
				resourceWF, err := Decode([]byte(capabilityResourceDocument))
				if err != nil {
					t.Fatal(err)
				}
				leaf.Container, leaf.Script, leaf.ContainerSet = nil, nil, nil
				leaf.Resource = resourceWF.Spec.Templates[0].Resource
				helper := wfv1.UserContainer{Container: corev1.Container{Name: "authored-helper", Image: image, Command: []string{"true"}}}
				if helpers != "sidecars" {
					leaf.InitContainers = []wfv1.UserContainer{helper}
				}
				if helpers != "initContainers" {
					leaf.Sidecars = []wfv1.UserContainer{helper}
				}
				if err := Admit(wf, capabilityPolicyForTest(t)); err == nil || !strings.Contains(err.Error(), refusal) {
					t.Fatalf("resource helper guard missing: %v", err)
				}
				// The resource-specific guard must disappear with the helpers, even when
				// an independent named-template rule still refuses this carrier.
				leaf.InitContainers, leaf.Sidecars = nil, nil
				err = Admit(wf, capabilityPolicyForTest(t))
				if err != nil && strings.Contains(err.Error(), refusal) {
					t.Fatalf("helper-free resource refused by helper guard: %v", err)
				}
				if carrier != "defaults" && carrier != "inline-dag" && carrier != "inline-steps" && err != nil {
					t.Fatalf("helper-free resource control: %v", err)
				}
				// Ordinary helpers keep their existing policy, including the native
				// generated mount checked at delivery; resource grants do not ban them.
				leaf.Resource = nil
				leaf.Container = &corev1.Container{Image: image, Command: []string{"true"}}
				leaf.InitContainers = []wfv1.UserContainer{helper}
				leaf.Sidecars = []wfv1.UserContainer{helper}
				err = Admit(wf, capabilityPolicyForTest(t))
				if err != nil && strings.Contains(err.Error(), refusal) {
					t.Fatalf("ordinary helpers hit resource guard: %v", err)
				}
				if carrier != "defaults" && err != nil {
					t.Fatalf("ordinary helper control: %v", err)
				}
			})
		}
	}
}
