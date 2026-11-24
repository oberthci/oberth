package installer

import (
	"encoding/json"

	"helm.sh/helm/v3/pkg/chart"
)

// Generated inputs follow the target chart's contract, including when the
// target is a previously released chart selected for rollback or repair.
func declaredChartValues(ch *chart.Chart, values map[string]any) (map[string]any, error) {
	if len(ch.Schema) == 0 {
		return values, nil
	}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(ch.Schema, &schema); err != nil {
		return nil, problem("chart", err)
	}
	declared := map[string]any{}
	for name, value := range values {
		if _, ok := schema.Properties[name]; ok {
			declared[name] = value
		}
	}
	return declared, nil
}
