package admissioncontract

import (
	"encoding/json"
	"testing"
)

func TestPlanTemplateIsPrivateEvidence(t *testing.T) {
	raw, err := json.Marshal(PlanEntry{Path: "step", Name: "display", Template: "private"})
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"plan_path":"step","name":"display","dynamic":false,"credential_intent":false}`
	if string(raw) != want {
		t.Fatalf("plan entry = %s, want %s", raw, want)
	}
}
