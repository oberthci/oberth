package store

import (
	"bytes"
	"database/sql"
	"encoding/json"

	"github.com/oberthci/oberth/internal/model"
)

func stepDiagnosticJSON(value *model.StepDiagnostic, runID string) any {
	if value == nil {
		return nil
	}
	if value.Validate() != nil || (value.Available && value.Container.RunID != runID) {
		value = model.UnavailableStepDiagnostic("invalid-termination")
	}
	body, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	return string(body)
}

func readStepDiagnostic(raw sql.NullString, runID string) *model.StepDiagnostic {
	if !raw.Valid {
		return model.UnavailableStepDiagnostic("not-recorded")
	}
	invalid := model.UnavailableStepDiagnostic("invalid-termination")
	if len(raw.String) > 4096 {
		return invalid
	}
	var value model.StepDiagnostic
	decoder := json.NewDecoder(bytes.NewBufferString(raw.String))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&value) != nil || value.Validate() != nil || (value.Available && value.Container.RunID != runID) {
		return invalid
	}
	// Writers emit canonical typed JSON. Exact equality rejects duplicate keys,
	// trailing data and alternate key spellings without retaining raw values.
	canonical, err := json.Marshal(value)
	if err != nil || string(canonical) != raw.String {
		return invalid
	}
	return &value
}
