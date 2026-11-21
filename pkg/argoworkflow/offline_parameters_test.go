package argoworkflow

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/sourcecontract"
)

func TestInspectionPlaceholdersRequireOfflineValidation(t *testing.T) {
	for _, offline := range []bool{false, true} {
		for _, address := range []string{"", "https://configured.invalid"} {
			w := simpleWorkflow()
			r := requestFor(t, w)
			r.Mode, r.IdentityRequest.Mode = admissioncontract.ModeInspect, admissioncontract.ModeInspect
			r.Trigger, r.ScheduleEntry = "schedule", "nightly"
			hooks := identityHooks()
			hooks.OfflineValidation = offline
			prepared, err := Prepare(t.Context(), r, Policy{SecretstoreAddress: address}, hooks)
			if err != nil {
				t.Fatal(err)
			}
			supplied := map[string]string{}
			for _, p := range prepared.View().Workflow.Spec.Arguments.Parameters {
				supplied[p.Name] = p.Value.String()
			}
			want := address
			if want == "" && offline {
				want = "https://secretstore.invalid"
			}
			value, present := supplied["oberth-secretstore-address"]
			if value != want || present != (address != "" || offline) || supplied["oberth-schedule"] != "nightly" {
				t.Fatal("server inspection acquired offline values", offline, address, supplied)
			}

		}
		assertOfflineParameterFallbacks(t, offline)
	}
}

// A schedule parent serves many runs, so its children carry the fixed scheduled
// locator; a run carries its own ID; a workflow without source carries none.
func TestScheduleParentStampsScheduledSourceLocator(t *testing.T) {
	for _, c := range []struct {
		mode    admissioncontract.Mode
		trigger string
		source  bool
		want    string
	}{
		{admissioncontract.ModeInspect, "schedule", true, sourcecontract.ScheduledLocator},
		{admissioncontract.ModeSubmit, "schedule", true, "run-1"},
		{admissioncontract.ModeSubmit, "branch", true, "run-1"},
		{admissioncontract.ModeInspect, "schedule", false, ""},
	} {
		w := simpleWorkflow()
		r := requestFor(t, w)
		r.Mode, r.Trigger, r.RunID = c.mode, model.RunTrigger(c.trigger), "run-1"
		if c.mode == admissioncontract.ModeInspect {
			r.RunID = ""
		}
		g := newGraph(t.Context(), w, r, Policy{})
		g.inventory = &sourceInventory{usesSource: c.source}
		g.stampParameters(w, r)
		value, present := "", false
		for _, p := range w.Spec.Arguments.Parameters {
			if p.Name == "oberth-source" {
				value, present = p.Value.String(), true
			}
		}
		if value != c.want || present != c.source {
			t.Fatal("source locator differs", c, value, present)
		}
	}
}

func assertOfflineParameterFallbacks(t *testing.T, offline bool) {
	t.Helper()
	w := simpleWorkflow()
	r := requestFor(t, w)
	r.Mode = admissioncontract.ModeInspect
	g := newGraph(t.Context(), w, r, Policy{})
	g.offlineValidation, g.inventory = offline, &sourceInventory{usesSource: true}
	r.RunID, r.Trigger = "", ""
	g.stampParameters(w, r)
	for _, p := range w.Spec.Arguments.Parameters {
		if p.Name != "oberth-source" && p.Name != "oberth-trigger" {
			continue
		}
		want := ""
		if offline {
			want = "branch"
			if p.Name == "oberth-source" {
				want = strings.Repeat("0", 32)
			}
		}
		if p.Value.String() != want {
			t.Fatal("placeholder escaped offline validation", p)
		}
	}
}
