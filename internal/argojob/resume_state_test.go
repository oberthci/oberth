package argojob_test

import (
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/argojob"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"reflect"
	"testing"
)

func TestResumeKeepsCrossStepState(t *testing.T) {
	t.Parallel()
	in := resumeFixture(t, resumeDAG)
	a := in.Workflow.Status.Nodes[in.Workflow.Name+"-1"]
	a.Outputs = &wf.Outputs{Parameters: []wf.Parameter{{Name: "version", Value: wf.AnyStringPtr("built-p0")}}, Artifacts: []wf.Artifact{{Name: "output", ArtifactLocation: wf.ArtifactLocation{Raw: &wf.RawArtifact{Data: "artifact-p0"}}}}}
	in.Workflow.Status.Nodes[a.ID] = a
	for _, w := range []*wf.Workflow{in.Workflow, in.Sealed.Workflow} {
		w.Spec.VolumeClaimTemplates = []core.PersistentVolumeClaim{{ObjectMeta: meta.ObjectMeta{Name: "work"}}}
		w.Spec.VolumeClaimGC = &wf.VolumeClaimGC{Strategy: wf.VolumeClaimGCOnSuccess}
	}
	in.Workflow.Status.PersistentVolumeClaims = []core.Volume{{Name: "work", VolumeSource: core.VolumeSource{PersistentVolumeClaim: &core.PersistentVolumeClaimVolumeSource{ClaimName: in.Workflow.Name + "-work"}}}}
	in.Sealed.Plan.Entries[0].OutputParameters = true
	plan, err := argojob.PlanResume(t.Context(), in)
	compositionOK(t, err)
	if !reflect.DeepEqual(plan.Workflow.Status.Nodes[a.ID].Outputs, a.Outputs) || plan.Workflow.Name != in.Workflow.Name || !reflect.DeepEqual(plan.Workflow.Spec.VolumeClaimTemplates, in.Workflow.Spec.VolumeClaimTemplates) || plan.Workflow.Spec.VolumeClaimGC.Strategy != wf.VolumeClaimGCOnSuccess || plan.Workflow.Name+"-"+plan.Workflow.Spec.VolumeClaimTemplates[0].Name != in.Workflow.Status.PersistentVolumeClaims[0].PersistentVolumeClaim.ClaimName {
		t.Fatal("retry lost passed outputs, artifacts or retained workspace identity")
	}
}
