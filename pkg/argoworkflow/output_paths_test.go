package argoworkflow

import (
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	core "k8s.io/api/core/v1"
)

func TestArtifactAndOutputParameterPathsRefuseParentComponents(t *testing.T) {
	t.Parallel()
	for _, shape := range []struct{ kind, inputPath string }{
		{"parameter", "/data/../file"},
		{"output artifact", "/data/link"},
		{"input artifact", "/data/link"},
	} {
		kind := shape.kind
		for _, tc := range []struct {
			value   string
			refused bool
		}{
			{"/data/link", false}, {"/data/..data", false}, {"/data/name..", false}, {"{{inputs.parameters.file}}", false},
			{"/data/../file", true}, {"/work/src/../../../var/run/secrets/kubernetes.io/serviceaccount/token", true}, {"../file", true}, {"/data/..", true},
		} {
			value := tc.value
			t.Run(kind+"/"+value, func(t *testing.T) {
				w := simpleWorkflow()
				tpl := &w.Spec.Templates[0]
				tpl.Inputs.Parameters = []wf.Parameter{{Name: "file", Default: wf.AnyStringPtr(shape.inputPath)}}
				tpl.Volumes = []core.Volume{{Name: "data", VolumeSource: core.VolumeSource{EmptyDir: &core.EmptyDirVolumeSource{}}}}
				tpl.Container.VolumeMounts = []core.VolumeMount{{Name: "data", MountPath: "/data"}}
				switch kind {
				case "parameter":
					tpl.Outputs.Parameters = []wf.Parameter{{Name: "value", ValueFrom: &wf.ValueFrom{Path: value}}}
				case "output artifact":
					tpl.Outputs.Artifacts = []wf.Artifact{{Name: "value", Path: value}}
				case "input artifact":
					// Parameter substitution has its own reserved-path refusal for inputs.
					if value == "{{inputs.parameters.file}}" {
						return
					}
					tpl.Inputs.Artifacts = []wf.Artifact{{Name: "value", Path: value, Optional: true}}
				}
				_, err := Prepare(t.Context(), requestFor(t, w), Policy{}, identityHooks())
				if tc.refused {
					requireCode(t, err, "path")
				} else if err != nil {
					t.Fatal("clean path refused", err)
				}
			})
		}
	}
}
