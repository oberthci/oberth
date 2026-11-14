package argojob

import (
	"encoding/json"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/argoproj/argo-workflows/v4/util/logging"
	"github.com/argoproj/argo-workflows/v4/workflow/common"
	corev1 "k8s.io/api/core/v1"
)

// Probe the exact pinned substitution path before considering a template JSON
// env inside a JSON PodSpecPatch string. No local interpolation is involved.
func TestExecutorTemplateNativeEncodingConstraint(t *testing.T) {
	for _, value := range []string{"/tmp/plain", `/tmp/quote"and\slash`} {
		t.Run(value, func(t *testing.T) {
			leaf := &wfv1.Template{Name: "main", Inputs: wfv1.Inputs{Parameters: []wfv1.Parameter{{Name: "path", Default: wfv1.AnyStringPtr(value)}}}, Container: &corev1.Container{Name: "main"}, Outputs: wfv1.Outputs{Parameters: []wfv1.Parameter{{Name: "out", ValueFrom: &wfv1.ValueFrom{Path: "{{inputs.parameters.path}}"}}}}}
			view := leaf.DeepCopy()
			view.Inputs = wfv1.Inputs{}
			encoded, err := json.Marshal(view)
			if err != nil {
				t.Fatal(err)
			}
			patch := corev1.PodSpec{Containers: []corev1.Container{{Name: "wait", Env: []corev1.EnvVar{{Name: "ARGO_TEMPLATE", Value: string(encoded)}}}}}
			encoded, err = json.Marshal(patch)
			if err != nil {
				t.Fatal(err)
			}
			leaf.PodSpecPatch = string(encoded)
			resolved, err := common.ProcessArgs(logging.TestContext(t.Context()), leaf, &wfv1.Arguments{}, common.Parameters{}, common.Parameters{}, false, "default", nil)
			if err != nil {
				t.Fatalf("pinned template processing: %v", err)
			}
			var actual corev1.PodSpec
			patchErr := json.Unmarshal([]byte(resolved.PodSpecPatch), &actual)
			var templateErr error
			var template wfv1.Template
			if patchErr == nil {
				templateErr = json.Unmarshal([]byte(actual.Containers[0].Env[0].Value), &template)
			}
			if value == "/tmp/plain" {
				if patchErr != nil || templateErr != nil {
					t.Fatalf("plain input: patch=%v template=%v", patchErr, templateErr)
				}
				if template.Outputs.Parameters[0].ValueFrom.Path != value {
					t.Fatal("plain substitution differs")
				}
			} else {
				if patchErr == nil && templateErr == nil {
					t.Fatal("encoding behavior changed: revisit native env feasibility")
				}
				t.Logf("native nested JSON encoding constraint: patch=%v template=%v", patchErr, templateErr)
			}
		})
	}
}
