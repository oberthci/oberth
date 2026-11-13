package argojob

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

// Restore only measurement fields from immutable captured input, for historical
// security proofs. No production submission calls this fixture reconstruction.
func historicalMeasurementInput(t *testing.T, current, captured *wfv1.Workflow) {
	t.Helper()
	const policy = "oberth.ci/step-usage-memory-policy"
	if value := captured.Annotations[policy]; value != "" {
		current.Annotations[policy] = value
	}
	var currentTemplates, capturedTemplates []*wfv1.Template
	walkTemplates(current, func(template *wfv1.Template) { currentTemplates = append(currentTemplates, template) })
	walkTemplates(captured, func(template *wfv1.Template) { capturedTemplates = append(capturedTemplates, template) })
	if len(currentTemplates) != len(capturedTemplates) {
		t.Fatal("historical template graph differs")
	}
	for i, template := range currentTemplates {
		original := capturedTemplates[i]
		if template.Name != original.Name {
			t.Fatal("historical template order differs")
		}
		const warning = "oberth.ci/step-usage-memory-warning"
		if value := original.Metadata.Annotations[warning]; value != "" {
			if template.Metadata.Annotations == nil {
				template.Metadata.Annotations = map[string]string{}
			}
			template.Metadata.Annotations[warning] = value
		}
		before, after := assistContainer(original), assistContainer(template)
		if before != nil && after != nil {
			if originalArgv := legacySecurityCommand(before.Command); len(originalArgv) != len(before.Command) {
				if !reflect.DeepEqual(originalArgv, after.Command) {
					t.Fatal("historical authored argv differs")
				}
				after.Command = append([]string(nil), before.Command...)
			}
			for _, mount := range before.VolumeMounts {
				if mount.MountPath == "/run/oberth/usage" {
					after.VolumeMounts = append(after.VolumeMounts, mount)
				}
			}
		}
		historicalMeasurementPatch(t, template, original)
	}
	identity, err := specIdentity(current)
	if err != nil {
		t.Fatal(err)
	}
	current.Annotations[identityAnnotation] = identity
}

func historicalMeasurementPatch(t *testing.T, current, captured *wfv1.Template) {
	t.Helper()
	var a, b map[string]any
	changed := false
	if captured.PodSpecPatch == "" {
		return
	}
	if err := json.Unmarshal([]byte(captured.PodSpecPatch), &b); err != nil {
		t.Fatal(err)
	}
	if current.PodSpecPatch != "" {
		if err := json.Unmarshal([]byte(current.PodSpecPatch), &a); err != nil {
			t.Fatal(err)
		}
	}
	if a == nil {
		a = map[string]any{}
	}
	if selectors, ok := b["nodeSelector"].(map[string]any); ok {
		if arch, ok := selectors[corev1.LabelArchStable]; ok {
			own, _ := a["nodeSelector"].(map[string]any)
			if own == nil {
				own = map[string]any{}
			}
			changed = true
			own[corev1.LabelArchStable] = arch
			a["nodeSelector"] = own
		}
	}
	original, _ := b["containers"].([]any)
	present, _ := a["containers"].([]any)
	for _, entry := range original {
		source := entry.(map[string]any)
		for _, targetEntry := range present {
			target := targetEntry.(map[string]any)
			if source["name"] != target["name"] {
				continue
			}
			mounts, _ := source["volumeMounts"].([]any)
			for _, entry := range mounts {
				mount := entry.(map[string]any)
				if location, ok := mount["mountPath"].(string); ok && strings.HasSuffix(location, "/run/oberth/usage") {
					own, _ := target["volumeMounts"].([]any)
					changed = true
					target["volumeMounts"] = append(own, mount)
				}
			}
		}
	}
	if !changed {
		return
	}
	encoded, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	current.PodSpecPatch = string(encoded)
}
