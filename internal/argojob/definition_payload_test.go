package argojob_test

import (
	"bytes"
	"encoding/json"
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/argoproj/argo-workflows/v4/util/instanceid"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"os"
	"testing"
)

func TestInstallerWorkflowPayload(t *testing.T) {
	templates := map[wf.TemplateType]bool{}
	calls := map[string]bool{}
	for _, sample := range []struct{ kind, name string }{
		{"definition-probe", "workflow"},
		{"script", "workflow-script"},
		{"container-set", "workflow-container-set"},
		{"suspend", "workflow-suspend"},
		{"resource", "workflow-resource"},
		{"named-dag", "workflow-named-dag"},
		{"named-steps", "workflow-named-steps"},
		{"reference-dag", "workflow-reference-dag"},
		{"reference-steps", "workflow-reference-steps"},
		{"review-inline-dag", "workflow-inline-dag"},
		{"review-inline-steps", "workflow-inline-steps"},
		{"review-inline-dag-script", "workflow-inline-dag-script"},
		{"review-inline-steps-script", "workflow-inline-steps-script"},
	} {
		t.Run(sample.name, func(t *testing.T) {
			p, e, native := fixtureKind(t, sample.kind)
			submitted := false
			native.BeforeCreate = func(w *wf.Workflow) {
				submitted = true
				requireSubmittedInstance(t, w)
			}
			if _, err := e.Create(t.Context(), p); err != nil {
				t.Fatal(err)
			}
			if !submitted || native.Posts != 1 || native.Patches != 0 {
				t.Fatal("instance label must be present on the first create, without a later patch")
			}
			w := native.Object.DeepCopy()
			requireSubmittedInstance(t, w)
			w.UID, w.ResourceVersion = "", ""
			raw, err := json.MarshalIndent(w, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			assertDefinitionPayload(t, sample.name, raw)
			root := &w.Spec.Templates[0]
			templates[root.GetType()] = true
			calls[string(root.GetType())+"/"+submittedInvocation(root)] = true
		})
	}
	requireAdmittedFormSamples(t, templates, calls)
}

func definitionProbeInput(w *wf.Workflow) {
	t := &w.Spec.Templates[0]
	t.Inputs.Artifacts = []wf.Artifact{{Name: "source", Path: "/work/src", Mode: ptr.To(int32(0755)), ArtifactLocation: wf.ArtifactLocation{Plugin: &wf.PluginArtifact{Name: "oberth", Key: argoworkflow.SourceArtifactKey, Configuration: `{"version":1,"kind":"main"}`}}}}
	w.Spec.Volumes = []core.Volume{{Name: "scratch", VolumeSource: core.VolumeSource{EmptyDir: &core.EmptyDirVolumeSource{}}}}
	t.Container.VolumeMounts = []core.VolumeMount{{Name: "scratch", MountPath: "/scratch"}}
	t.Container.WorkingDir = "/work/src"
	t.Outputs.Parameters = []wf.Parameter{{Name: "value", ValueFrom: &wf.ValueFrom{Path: "/scratch/value"}}}
}

func TestInstallerTaskResultPayload(t *testing.T) {
	// The native executor submits NodeResult at the root of WorkflowTaskResult.
	result := &wf.WorkflowTaskResult{TypeMeta: meta.TypeMeta{APIVersion: "argoproj.io/v1alpha1", Kind: "WorkflowTaskResult"}, ObjectMeta: meta.ObjectMeta{Name: "run-node", Namespace: "ci"}}
	// The pinned executor copies ARGO_INSTANCE_ID into this native label.
	instanceid.NewService("frontdoor").Label(result)
	result.Outputs = &wf.Outputs{Result: ptr.To("done"), ExitCode: ptr.To("0"), Parameters: []wf.Parameter{{Name: "value", Value: wf.AnyStringPtr("built")}}}
	result.Outputs.Artifacts = []wf.Artifact{{Name: "log", ArtifactLocation: wf.ArtifactLocation{S3: &wf.S3Artifact{Key: "run/log", S3Bucket: wf.S3Bucket{Bucket: "logs"}}}}}

	raw, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	assertDefinitionPayload(t, "task-result", raw)
}

func assertDefinitionPayload(t *testing.T, name string, raw []byte) {
	t.Helper()
	want, err := os.ReadFile("../installer/testdata/" + name + "-payload.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, want) {
		t.Fatal("installer compatibility payload differs from native submission", name)
	}
}

func requireSubmittedInstance(t *testing.T, object meta.Object) {
	t.Helper()
	if err := instanceid.NewService("frontdoor").Validate(object); err != nil {
		t.Fatal("Workflow visible without installation instance ID", err)
	}
	if instanceid.NewService("").Validate(object) == nil {
		t.Fatal("default controller selects submitted Workflow")
	}
}
