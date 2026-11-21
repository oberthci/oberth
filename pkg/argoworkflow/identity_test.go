package argoworkflow

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"k8s.io/utils/ptr"
)

func TestIdentityBindsPipelineAccountAcrossNativeFields(t *testing.T) {
	t.Parallel()
	wf := simpleWorkflow()
	decorate := func(t *wfv1.Template) {
		t.ServiceAccountName = "authored"
		t.AutomountServiceAccountToken = ptr.To(true)
		t.Executor = &wfv1.ExecutorConfig{ServiceAccountName: "authored"}
		t.Inputs.Artifacts = []wfv1.Artifact{{Name: "input", ArtifactGC: &wfv1.ArtifactGC{ServiceAccountName: "authored"}}}
	}
	decorate(&wf.Spec.Templates[0])
	wf.Spec.TemplateDefaults = wf.Spec.Templates[0].DeepCopy()
	inline := wf.Spec.Templates[0].DeepCopy()
	wf.Spec.Templates[0].DAG = &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "inline", Inline: inline}}}
	wf.Spec.Templates[0].Steps = []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{Name: "inline", Inline: inline.DeepCopy()}}}}
	wf.Spec.ServiceAccountName = "authored"
	wf.Spec.AutomountServiceAccountToken = ptr.To(true)
	wf.Spec.ArtifactGC = &wfv1.WorkflowLevelArtifactGC{ArtifactGC: wfv1.ArtifactGC{ServiceAccountName: "authored"}}
	wf.Spec.Hooks = wfv1.LifecycleHooks{"exit": {Arguments: wfv1.Arguments{Artifacts: []wfv1.Artifact{{ArtifactGC: &wfv1.ArtifactGC{ServiceAccountName: "authored"}}}}}}
	e := admissioncontract.IdentityEvidence{Namespace: "ci", Pipeline: admissioncontract.AccountEvidence{Name: "pipeline"}}
	bindIdentity(wf, e)
	raw, err := json.Marshal(wf)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "authored") || strings.Contains(string(raw), `"automountServiceAccountToken":true`) {
		t.Fatal(string(raw))
	}
	if wf.Spec.Executor.ServiceAccountName != "pipeline" || wf.Spec.Templates[0].DAG.Tasks[0].Inline.Executor.ServiceAccountName != "pipeline" || wf.Spec.ServiceAccountName != "pipeline" {
		t.Fatal("pipeline account did not bind every native executor")
	}
}
func TestIdentityFieldOwnersAreKnownInThePinnedSchema(t *testing.T) {
	t.Parallel()
	owners := map[string]bool{}
	identityOwners(reflect.TypeFor[wfv1.WorkflowSpec](), map[reflect.Type]bool{}, owners)
	got := sortedKeys(owners)
	want := []string{"ArtifactGC.ServiceAccountName", "ExecutorConfig.ServiceAccountName", "Template.AutomountServiceAccountToken", "Template.ServiceAccountName", "WorkflowSpec.AutomountServiceAccountToken", "WorkflowSpec.ServiceAccountName"}
	slices.Sort(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("native identity surface changed: %v", got)
	}
}
func identityOwners(typ reflect.Type, seen map[reflect.Type]bool, owners map[string]bool) {
	for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice || typ.Kind() == reflect.Map || typ.Kind() == reflect.Array {
		typ = typ.Elem()
	}
	if typ.Kind() != reflect.Struct || seen[typ] {
		return
	}
	seen[typ] = true
	for i := range typ.NumField() {
		field := typ.Field(i)
		if !field.IsExported() {
			continue
		}
		if field.Name == "ServiceAccountName" || field.Name == "AutomountServiceAccountToken" {
			owners[typ.Name()+"."+field.Name] = true
		}
		identityOwners(field.Type, seen, owners)
	}
}
