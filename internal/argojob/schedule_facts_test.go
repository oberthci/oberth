package argojob_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	jsonpatch "github.com/evanphx/json-patch/v5"
	"github.com/oberthci/oberth/internal/cronsync"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func TestScheduleProducerMatchesChildRequirements(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, address string }{{"default", ""}, {"secret-address", "https://secrets.oberth.ci"}} {
		t.Run(tc.name, func(t *testing.T) {
			assertScheduleFactContract(t, realScheduleFixture(t, argoworkflow.Policy{SecretstoreAddress: tc.address}))
		})
	}
}

func assertScheduleFactContract(t *testing.T, f *childFixture) {
	t.Helper()
	produced := scheduleProducedFacts(t, f.prepared.View().Workflow)
	var got []string
	for name := range produced {
		got = append(got, name)
	}
	slices.Sort(got)
	want := requiredScheduleFacts()
	slices.Sort(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("producer/check fact lists differ: got %v; want %v", got, want)
	}
	if _, found, _, err := f.syncer.ChildBinding(t.Context(), f.child); err != nil || !found {
		t.Fatal("real prepared child refused", found, err)
	}
	if f.child.Spec.WorkflowMetadata.Labels["authored"] != "workflow" || f.child.Spec.PodMetadata.Labels["authored"] != "pod" || f.child.Spec.WorkflowMetadata.Annotations["note"] != "workflow" || f.child.Spec.PodMetadata.Annotations["note"] != "pod" {
		t.Fatal("preparation replaced authored metadata")
	}
	for _, name := range got {
		t.Run(name, func(t *testing.T) {
			candidate := removeScheduleFact(t, f.child, produced[name])
			_, found, _, err := f.syncer.ChildBinding(t.Context(), candidate)
			var refusal *cronsync.ChildRefusal
			if found || !errors.As(err, &refusal) {
				t.Fatal("child check did not require produced fact", name, found, err)
			}
		})
	}
}

func requiredScheduleFacts() []string {
	facts := []string{
		"metadata.annotations[" + nativebinding.RefAnnotation + "]",
		"spec.workflowMetadata.labelsFrom[" + nativebinding.RunLabel + "]",
	}
	for _, group := range []string{"metadata.labels"} {
		for _, key := range []string{nativebinding.EngineLabel, nativebinding.RepositoryLabel, nativebinding.CommitLabel, nativebinding.TriggerLabel, nativebinding.TierLabel} {
			facts = append(facts, group+"["+key+"]")
		}
	}
	for _, key := range []string{"oberth-ref", "oberth-sha", "oberth-trigger", "oberth-schedule"} {
		facts = append(facts, "spec.arguments.parameters["+key+"]")
	}
	return facts
}

func scheduleProducedFacts(t *testing.T, w *wf.Workflow) map[string]string {
	t.Helper()
	raw, err := json.Marshal(w)
	compositionOK(t, err)
	var object map[string]any
	compositionOK(t, json.Unmarshal(raw, &object))
	facts := map[string]string{}
	for _, group := range []string{"metadata.labels", "metadata.annotations", "spec.workflowMetadata.labels", "spec.workflowMetadata.labelsFrom", "spec.workflowMetadata.annotations", "spec.podMetadata.labels", "spec.podMetadata.annotations"} {
		current := object
		for _, part := range strings.Split(group, ".") {
			current, _ = current[part].(map[string]any)
		}
		for key := range current {
			if strings.HasPrefix(key, "oberth.ci/") {
				facts[group+"["+key+"]"] = "/" + strings.ReplaceAll(group, ".", "/") + "/" + strings.ReplaceAll(key, "/", "~1")
			}
		}
	}
	for i, p := range w.Spec.Arguments.Parameters {
		if slices.Contains([]string{"oberth-ref", "oberth-sha", "oberth-trigger", "oberth-schedule"}, p.Name) {
			facts["spec.arguments.parameters["+p.Name+"]"] = "/spec/arguments/parameters/" + strconv.Itoa(i)
		}
	}
	return facts
}

func removeScheduleFact(t *testing.T, w *wf.Workflow, path string) *wf.Workflow {
	t.Helper()
	raw, err := json.Marshal(w)
	compositionOK(t, err)
	operations, err := json.Marshal([]map[string]string{{"op": "remove", "path": path}})
	compositionOK(t, err)
	patch, err := jsonpatch.DecodePatch(operations)
	compositionOK(t, err)
	raw, err = patch.Apply(raw)
	compositionOK(t, err)
	var candidate wf.Workflow
	compositionOK(t, json.Unmarshal(raw, &candidate))
	return &candidate
}
