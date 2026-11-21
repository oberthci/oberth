package publicerr

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestRequiredContextSurvivesClassification(t *testing.T) {
	first := New(Forbidden, "Host is not trusted.", "Verify its fingerprint.", errors.New("private key material"))
	first.Context = []string{"Offered fingerprint: SHA256:example"}
	first.Detail = "Optional diagnostics."
	second := New(Forbidden, "Other host is not trusted.", "Verify the other fingerprint.", nil)
	second.Context = []string{"Other offered fingerprint: SHA256:other"}
	problem := Classify(errors.Join(first, second, first))
	encoded, err := json.Marshal(problem)
	if err != nil || len(problem.Context) != 2 || strings.Contains(string(encoded), "private key material") || !strings.Contains(string(encoded), `"context":[`) || problem.Detail != first.Detail {
		t.Fatal("public/private context boundary changed", string(encoded), err)
	}
}

func TestRefusalIdentifierSurvivesClassificationAndWire(t *testing.T) {
	first := New(Forbidden, "Host is not trusted.", "Verify its fingerprint.", nil)
	first.ID = "host_trust"
	encoded, err := json.Marshal(Classify(errors.Join(first, first)))
	if err != nil || !strings.Contains(string(encoded), `"id":"host_trust"`) {
		t.Fatal("identifier lost", string(encoded), err)
	}
	second := New(Forbidden, "Another refusal.", "Correct the request.", nil)
	if Classify(errors.Join(first, second)).ID != "" {
		t.Fatal("mixed refusals acquired a subtype")
	}
}
