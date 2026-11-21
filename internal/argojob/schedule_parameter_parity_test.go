package argojob_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func TestScheduleAndOfflineShareParameterCatalogue(t *testing.T) {
	t.Run("schedule-parent", assertScheduleParameterParity)
	t.Run("offline-store-need", assertOfflineStoreConfigurationIsUnverified)
}

func assertScheduleParameterParity(t *testing.T) {
	t.Helper()
	seed := realScheduleFixture(t).prepared.View()
	authored, err := argoworkflow.Decode([]byte(scheduleWorkflow))
	compositionOK(t, err)
	for _, p := range seed.Workflow.Spec.Arguments.Parameters {
		if p.Name == "oberth-secretstore-address" {
			t.Fatal("server inspection stamped an offline address", p)
		}
		authored.Spec.Templates[0].Container.Args = append(authored.Spec.Templates[0].Container.Args, "{{workflow.parameters."+p.Name+"}}")
	}
	raw, err := json.Marshal(authored)
	compositionOK(t, err)
	f := realScheduleDocument(t, string(raw))
	parent := f.prepared.View()
	assertScheduleFactContract(t, f)
	r := admissioncontract.PrepareRequest{Mode: admissioncontract.ModeInspect, Trigger: "schedule", ScheduleEntry: "nightly", SelectedFile: ".oberth/build.yaml", Document: raw}
	offline, err := argoworkflow.Prepare(t.Context(), r, argoworkflow.Policy{}, admissioncontract.PrepareHooks{OfflineValidation: true})
	compositionOK(t, err)
	supplied := map[string]string{}
	for _, p := range offline.View().Workflow.Spec.Arguments.Parameters {
		supplied[p.Name] = p.Value.String()
	}
	for _, p := range parent.Workflow.Spec.Arguments.Parameters {
		if supplied[p.Name] == "" {
			t.Fatal("schedule parameter is unknown offline", p.Name)
		}
	}
	if !strings.HasPrefix(supplied["oberth-ref"], "refs/heads/") || len(supplied["oberth-sha"]) != 40 {
		t.Fatal("offline schedule facts have the wrong shape", supplied)
	}
}

func assertOfflineStoreConfigurationIsUnverified(t *testing.T) {
	t.Helper()
	w, err := argoworkflow.Decode([]byte(scheduleWorkflow))
	compositionOK(t, err)
	w.Spec.Templates[0].Metadata.Annotations = map[string]string{argoworkflow.SecretPathsAnnotation: "ci/token"}
	w.Spec.Templates[0].Container.Command = []string{"oberth", "secretstore", "exec"}
	raw, err := json.Marshal(w)
	compositionOK(t, err)
	r := admissioncontract.PrepareRequest{Mode: admissioncontract.ModeInspect, Trigger: "branch", SelectedFile: ".oberth/build.yaml", Document: raw}
	prepared, err := argoworkflow.Prepare(t.Context(), r, argoworkflow.Policy{}, admissioncontract.PrepareHooks{OfflineValidation: true})
	compositionOK(t, err)
	found := false
	for _, f := range prepared.View().Findings {
		if strings.Contains(f.Reason, "not configured") {
			t.Fatal("offline validation asserted installation facts", f)
		}
		if f.Code == "store_unverified" {
			found = true
			if f.Reason != "Secret store configuration is unverified" || f.Command != "oberth secretstore check" {
				t.Fatal("offline store need lost its uncertainty or remedy", f)
			}
		}
	}
	if !found {
		t.Fatal("offline store check disappeared", prepared.View().Findings)
	}
}
