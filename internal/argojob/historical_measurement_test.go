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
			if len(before.Command) > 3 && before.Command[0] == "/run/oberth/usage/oberth" && before.Command[1] == "internal-step-exec" && before.Command[2] == "--" {
				originalArgv := before.Command[3:]
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

// Captures retain their original helper budgets. Compare current security and
// mount behavior against them without requiring an old resource-policy replay.
func withoutCapturedHelperBudgets(t *testing.T, pod *corev1.Pod) {
	t.Helper()
	helper := func(name string) bool { return name == "init" || name == "wait" || name == executorTokenWaiter }
	for _, containers := range [][]corev1.Container{pod.Spec.InitContainers, pod.Spec.Containers} {
		for i := range containers {
			c := &containers[i]
			if helper(c.Name) {
				c.Resources = corev1.ResourceRequirements{}
			}
			for j := range c.Env {
				env := &c.Env[j]
				if env.Name != "ARGO_TEMPLATE" {
					continue
				}
				var template wfv1.Template
				if err := json.Unmarshal([]byte(env.Value), &template); err != nil {
					t.Fatal(err)
				}
				var patch map[string]any
				if err := json.Unmarshal([]byte(template.PodSpecPatch), &patch); err != nil {
					t.Fatal(err)
				}
				for _, group := range []string{"containers", "initContainers"} {
					items, _ := patch[group].([]any)
					var retained []any
					for _, item := range items {
						entry := item.(map[string]any)
						name, _ := entry["name"].(string)
						if helper(name) {
							delete(entry, "resources")
							if len(entry) == 1 {
								continue
							}
						}
						retained = append(retained, item)
					}
					if len(retained) == 0 {
						delete(patch, group)
					} else {
						patch[group] = retained
					}
				}
				encoded, err := json.Marshal(patch)
				if err != nil {
					t.Fatal(err)
				}
				template.PodSpecPatch = string(encoded)
				encoded, err = json.Marshal(template)
				if err != nil {
					t.Fatal(err)
				}
				env.Value = string(encoded)
			}
		}
	}
}

// Frozen controller captures predate native run-fact parameters. Remove only
// the retired fact env when comparing security behavior; captures stay intact.
func withoutCapturedRunFactEnvironment(t *testing.T, pod *corev1.Pod) {
	t.Helper()
	strip := withoutRunFactEnvironment
	for _, containers := range [][]corev1.Container{pod.Spec.InitContainers, pod.Spec.Containers} {
		for i := range containers {
			c := &containers[i]
			strip(c)
			for j := range c.Env {
				env := &c.Env[j]
				if env.Name != "ARGO_TEMPLATE" {
					continue
				}
				var template wfv1.Template
				if err := json.Unmarshal([]byte(env.Value), &template); err != nil {
					t.Fatal(err)
				}
				walkTemplate(&template, func(t *wfv1.Template) { templateContainers(t, strip) }, 0)
				encoded, err := json.Marshal(template)
				if err != nil {
					t.Fatal(err)
				}
				env.Value = string(encoded)
			}
		}
	}
}

func withoutRunFactEnvironment(c *corev1.Container) {
	kept := c.Env[:0]
	for _, env := range c.Env {
		switch env.Name {
		case "OBERTH_REPO", "OBERTH_REF", "OBERTH_SHA", "OBERTH_TRIGGER", "OBERTH_RUN_ID", "OBERTH_RELEASE_TAG", "OBERTH_RELEASE_SHA", "OBERTH_STEP":
			continue
		}
		kept = append(kept, env)
	}
	c.Env = kept
}

// Compare immutable controller inputs after removing only the retired run-fact
// interface. Every secret, helper, identity and security field still compares.
func withoutCapturedWorkflowRunFacts(t *testing.T, workflow *wfv1.Workflow) {
	t.Helper()
	parameters := workflow.Spec.Arguments.Parameters[:0:0]
	for _, parameter := range workflow.Spec.Arguments.Parameters {
		switch parameter.Name {
		case "oberth-repo", "oberth-ref", "oberth-sha", "oberth-trigger", "oberth-release-tag", "oberth-release-sha":
			continue
		}
		parameters = append(parameters, parameter)
	}
	workflow.Spec.Arguments.Parameters = parameters
	walkTemplates(workflow, func(template *wfv1.Template) { templateContainers(template, withoutRunFactEnvironment) })
	identity, err := specIdentity(workflow)
	if err != nil {
		t.Fatal(err)
	}
	workflow.Annotations[identityAnnotation] = identity
}
