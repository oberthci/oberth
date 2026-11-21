package argoworkflow

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/admissioncontract"
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
