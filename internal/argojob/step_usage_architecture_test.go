package argojob

import (
	"encoding/json"
	"runtime"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestAuthoredArchitectureSurvivesControllerCreateIncludingPodPatch(t *testing.T) {
	other := "amd64"
	if runtime.GOARCH == other {
		other = "arm64"
	}
	for _, site := range []string{"workflow-selector", "template-selector", "workflow-affinity", "template-affinity", "preferred-affinity", "mixed-affinity", "default-selector", "parent-selector"} {
		t.Run(site, func(t *testing.T) {
			wf := nativeWorkflow(t)
			wf.Spec.Templates[0].Container.Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}
			affinity := &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: corev1.LabelArchStable, Operator: corev1.NodeSelectorOpIn, Values: []string{other}}}}}}}}
			switch site {
			case "workflow-selector":
				wf.Spec.NodeSelector = map[string]string{corev1.LabelArchStable: other}
			case "template-selector":
				wf.Spec.Templates[0].NodeSelector = map[string]string{corev1.LabelArchStable: other}
			case "workflow-affinity":
				wf.Spec.Affinity = affinity
			case "template-affinity":
				wf.Spec.Templates[0].Affinity = affinity
			case "preferred-affinity":
				wf.Spec.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{PreferredDuringSchedulingIgnoredDuringExecution: []corev1.PreferredSchedulingTerm{{Weight: 1, Preference: affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0]}}}}
			case "mixed-affinity":
				affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions[0].Values = append([]string{runtime.GOARCH}, other)
				wf.Spec.Affinity = affinity
			case "default-selector":
				wf.Spec.TemplateDefaults = wf.Spec.Templates[0].DeepCopy()
				wf.Spec.TemplateDefaults.NodeSelector = map[string]string{corev1.LabelArchStable: other}
			case "parent-selector":
				wf.Spec.Templates = append(wf.Spec.Templates, *wf.Spec.Templates[0].DeepCopy())
				wf.Spec.Templates[1].Name = "parent"
				wf.Spec.Templates[1].NodeSelector = map[string]string{corev1.LabelArchStable: other}
			}
			before, _ := json.Marshal(wf.Spec)
			submitted := nativeSubmit(t, wf)
			for i, template := range submitted.Spec.Templates {
				var patch corev1.PodSpec
				if template.PodSpecPatch != "" {
					if err := json.Unmarshal([]byte(template.PodSpecPatch), &patch); err != nil {
						t.Fatal(err)
					}
				}
				if _, added := patch.NodeSelector[corev1.LabelArchStable]; added {
					t.Fatalf("server narrowed authored architecture via podSpecPatch: %s", template.PodSpecPatch)
				}
				if template.Container != nil && strings.Contains(strings.Join(template.Container.Command, " "), usageCommand) {
					t.Fatal("incompatible executable was injected")
				}
				if nativePatchPlacement(t, template.PodSpecPatch) != nativePatchPlacement(t, wf.Spec.Templates[i].PodSpecPatch) {
					t.Fatalf("Pod patch changed: %q -> %q", wf.Spec.Templates[i].PodSpecPatch, template.PodSpecPatch)
				}
			}
			// Compare authored placement separately from expected identity/helper fields.
			nativeEqual(t, wf.Spec.NodeSelector, submitted.Spec.NodeSelector)
			nativeEqual(t, wf.Spec.Affinity, submitted.Spec.Affinity)
			for i := range wf.Spec.Templates {
				nativeEqual(t, wf.Spec.Templates[i].NodeSelector, submitted.Spec.Templates[i].NodeSelector)
				nativeEqual(t, wf.Spec.Templates[i].Affinity, submitted.Spec.Templates[i].Affinity)
			}
			after, _ := json.Marshal(wf.Spec)
			if string(before) != string(after) {
				t.Fatal("source object was mutated")
			}
		})
	}
}

func TestCompatibleAuthoredArchitectureStillMeasures(t *testing.T) {
	wf := nativeWorkflow(t)
	wf.Spec.NodeSelector = map[string]string{corev1.LabelArchStable: runtime.GOARCH}
	wf.Spec.Templates[0].Container.Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}
	submitted := nativeSubmit(t, wf)
	if !strings.Contains(strings.Join(submitted.Spec.Templates[0].Container.Command, " "), usageCommand) {
		t.Fatal("compatible step lost measurement")
	}
	nativeEqual(t, wf.Spec.NodeSelector, submitted.Spec.NodeSelector)
}
