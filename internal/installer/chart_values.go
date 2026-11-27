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
		if definition, ok := schema.Properties[name]; ok {
			declared[name] = declaredObjectValues(definition, value)
		}
	}
	return declared, nil
}

// Closed object fields follow the target contract too: a prior chart need not
// accept a later chart's nested choices. Open native resource dictionaries retain
// their keys. Retained native PriorityClasses preserve referenced names on rollback.
func declaredObjectValues(raw json.RawMessage, value any) any {
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Additional json.RawMessage            `json:"additionalProperties"`
		Patterns   map[string]json.RawMessage `json:"patternProperties"`
	}
	object, ok := value.(map[string]any)
	if !ok || json.Unmarshal(raw, &schema) != nil || string(schema.Additional) != "false" || len(schema.Patterns) > 0 {
		return value
	}
	result := map[string]any{}
	for name, item := range object {
		if field, found := schema.Properties[name]; found {
			result[name] = declaredObjectValues(field, item)
		}
	}
	return result
}
