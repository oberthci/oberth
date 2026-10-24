package argojob

import (
	"context"
	"strings"
	"testing"

	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

func TestScheduleEntryMaterializationUsesDurableIdentity(t *testing.T) {
	source := strings.Replace(greedyDocument, "/bin/true", "/bin/echo", 1)
	for _, entry := range []string{"nightly", "weekly-sweep"} {
		request := testRequest(periapsis.TriggerCI, source)
		request.RunTrigger = argoworkflow.RunTriggerSchedule
		request.ScheduleEntry = entry
		workflow, err := Build(testConfig(), request)
		if err != nil {
			t.Fatal(err)
		}
		if !argoworkflow.ScheduleEntryMatches(workflow, request.RunTrigger, entry) {
			t.Fatalf("missing durable schedule %s", entry)
		}
		if argoworkflow.ScheduleEntryMatches(workflow, request.RunTrigger, "changed-file-entry") {
			t.Fatal("different entry adopted")
		}
		workflow.UID = "server-uid"
		controller := &Controller{workflows: &staticWorkflowClient{workflow: workflow}, config: testConfig()}
		adoption := AdoptionRequest{Name: request.Name, RunID: request.RunID, Ref: request.Ref, SHA: request.SHA, Trigger: request.Trigger, RunTrigger: request.RunTrigger, ScheduleEntry: entry}
		if _, found, err := controller.Adopt(context.Background(), adoption); err != nil || !found {
			t.Fatalf("schedule adoption: %v/%v", found, err)
		}
		adoption.ScheduleEntry = "other-entry"
		if _, _, err := controller.Adopt(context.Background(), adoption); err == nil {
			t.Fatal("durable entry mismatch admitted after restart")
		}
	}
}

func TestScheduleEntryCannotBeForgedByNonScheduleRuns(t *testing.T) {
	for _, trigger := range []argoworkflow.RunTrigger{argoworkflow.RunTriggerBranch, argoworkflow.RunTriggerPromotion, argoworkflow.RunTriggerTag} {
		tier := periapsis.TriggerCI
		if trigger == argoworkflow.RunTriggerTag {
			tier = periapsis.TriggerRelease
		}
		request := testRequest(tier, strings.Replace(greedyDocument, "value: yes-please", "value: \"{{workflow.parameters.oberth-schedule}}\"", 1))
		request.RunTrigger = trigger
		workflow, err := Build(testConfig(), request)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range workflow.Spec.Arguments.Parameters {
			if p.Name == argoworkflow.ScheduleParameter && p.Value.String() != "" {
				t.Fatal("non-schedule inherited entry")
			}
		}
		request.ScheduleEntry = "nightly"
		if _, err := Build(testConfig(), request); err == nil {
			t.Fatalf("%s forged schedule identity", trigger)
		}
	}
	for _, value := range []string{"nightly", "''", "\"{{workflow.parameters.fake}}\""} {
		source := strings.Replace(greedyDocument, "spec:\n", "spec:\n  arguments:\n    parameters:\n    - name: oberth-schedule\n      value: "+value+"\n", 1)
		if _, err := Build(testConfig(), testRequest(periapsis.TriggerCI, source)); err == nil || !strings.Contains(err.Error(), "server-owned") {
			t.Fatalf("repository forged schedule default %s: %v", value, err)
		}
	}
}
