package installer

import (
	"encoding/json"
	"testing"

	"helm.sh/helm/v3/pkg/chart/loader"
)

func TestAllowedOverridesAreDeclaredByChart(t *testing.T) {
	ch, err := loader.Load("../../charts/oberth")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err = json.Unmarshal(ch.Schema, &schema); err != nil {
		t.Fatal(err)
	}
	for _, name := range allowedOverrideNames() {
		if _, declared := schema.Properties[name]; !declared {
			t.Errorf("allowed override is absent from chart schema: %s", name)
		}
	}
}
