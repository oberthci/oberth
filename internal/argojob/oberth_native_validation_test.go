package argojob

import (
	"strings"
	"testing"

	"github.com/argoproj/argo-workflows/v4/util/logging"
	"github.com/argoproj/argo-workflows/v4/workflow/validate"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/oberthci/oberth/pkg/pipeline"
)

// Decode and Oberth admission do not enforce every controller semantic rule.
// Validate the final submission with the same Argo module as the managed
// controller (chart 1.0.24 / v4.0.8), including release-only templates during
// branch CI. Do not use lint mode: submission requires resolved arguments.
func TestOberthPipelinesPassNativeArgoValidation(t *testing.T) {
	for _, tc := range []struct {
		file    string
		trigger pipeline.Trigger
		ref     string
	}{
		{"build.yaml", pipeline.TriggerCI, "refs/heads/main"},
		{"release.yaml", pipeline.TriggerRelease, "refs/tags/v0.1.0"},
	} {
		t.Run(tc.file, func(t *testing.T) {
			workflow, err := Build(oberthConfig(), Request{
				RunID: "run-native", Name: "oberth-native-validation",
				Repo: "oberth", UpstreamOrg: "acme", UpstreamName: "github",
				Ref: tc.ref, SHA: testSHA, Trigger: tc.trigger,
				Source: loadPipeline(t, tc.file), ApprovedSecrets: oberthReleaseApprovedSecrets(),
			})
			if err != nil {
				t.Fatal(err)
			}
			ctx := logging.TestContext(t.Context())
			if err := validate.ValidateWorkflow(ctx, nil, nil, workflow, nil, validate.ValidateOpts{Submit: true}); err != nil {
				t.Fatalf("final %s submission fails native Argo validation: %v", tc.file, err)
			}

			// Reintroduce the failed release's field on every DAG and Steps
			// template. The actual upstream validator must reject each case;
			// stripping all deadlines before validation would hide the defect.
			for i, tmpl := range workflow.Spec.Templates {
				if tmpl.DAG == nil && len(tmpl.Steps) == 0 {
					continue
				}
				t.Run(tmpl.Name+"/nonleaf-deadline", func(t *testing.T) {
					invalid := workflow.DeepCopy()
					deadline := intstr.FromInt(7401)
					invalid.Spec.Templates[i].ActiveDeadlineSeconds = &deadline
					err := validate.ValidateWorkflow(ctx, nil, nil, invalid, nil, validate.ValidateOpts{Submit: true})
					want := "templates." + tmpl.Name + ".activeDeadlineSeconds is only valid for leaf templates"
					if err == nil || !strings.Contains(err.Error(), want) {
						t.Fatalf("native validation error = %v, want %q", err, want)
					}
				})
			}
		})
	}
}
