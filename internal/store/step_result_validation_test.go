package store

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestPutStepResultNamesInvalidField(t *testing.T) {
	now := time.Unix(1000, 0).UTC()
	s := testStore(t, &now)
	for _, tc := range []struct {
		name, field string
		mutate      func(*model.StepResult)
	}{
		{"run", "run_id", func(r *model.StepResult) { r.RunID = " " }},
		{"node", "node_id", func(r *model.StepResult) { r.NodeID = " " }},
		{"ordinal", "ordinal", func(r *model.StepResult) { r.Ordinal = -1 }},
		{"pending", "status", func(r *model.StepResult) { r.Status = model.StepPending }},
		{"unknown-status", "status", func(r *model.StepResult) { r.Status = "unknown-sensitive-value" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := model.StepResult{NodeID: "test.unit", RunID: "run", Stage: "test", Step: "unit", Status: model.StepPassed}
			tc.mutate(&result)
			_, err := s.PutStepResult(t.Context(), result)
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "invalid field "+tc.field) || strings.Contains(err.Error(), "unknown-sensitive-value") {
				t.Fatalf("error=%v, want field %s without raw values", err, tc.field)
			}
		})
	}
}
