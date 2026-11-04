package store

import (
	"errors"
	"strings"
	"testing"
	"time"

	"k8s.io/utils/ptr"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/pipeline"
)

func TestPutStepResultNamesInvalidField(t *testing.T) {
	now := time.Unix(1000, 0).UTC()
	s := testStore(t, &now)
	for _, tc := range []struct {
		name, field string
		mutate      func(*model.StepResult)
	}{
		{"run", "run_id", func(r *model.StepResult) { r.RunID = " " }},
		{"stage", "stage", func(r *model.StepResult) { r.Stage = " " }},
		{"step", "step", func(r *model.StepResult) { r.Step = " " }},
		{"ordinal", "ordinal", func(r *model.StepResult) { r.Ordinal = -1 }},
		{"pending", "status", func(r *model.StepResult) { r.Status = model.StepPending }},
		{"queued", "status", func(r *model.StepResult) { r.Status = model.StepQueued }},
		{"running", "status", func(r *model.StepResult) { r.Status = model.StepRunning }},
		{"unknown-status", "status", func(r *model.StepResult) { r.Status = "unknown-sensitive-value" }},
		{"log-start", "log_start", func(r *model.StepResult) { r.LogStart = -1 }},
		{"log-end", "log_end", func(r *model.StepResult) { r.LogStart = 2; r.LogEnd = 1 }},
		{"missing-size", "declared_size", func(r *model.StepResult) { r.DeclaredSize = "" }},
		{"bad-size", "declared_size", func(r *model.StepResult) { r.DeclaredSize = pipeline.Size("unknown-sensitive-value") }},
		{"rss", "max_rss_bytes", func(r *model.StepResult) { r.MaxRSSBytes = ptr.To(int64(-1)) }},
		{"memory", "memory_peak_bytes", func(r *model.StepResult) { r.MemoryPeakBytes = ptr.To(int64(-1)) }},
		{"user", "user_cpu", func(r *model.StepResult) { r.UserCPU = ptr.To(time.Duration(-1)) }},
		{"system", "system_cpu", func(r *model.StepResult) { r.SystemCPU = ptr.To(time.Duration(-1)) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := model.StepResult{RunID: "run", Stage: "test", Step: "unit", Status: model.StepPassed, DeclaredSize: pipeline.M}
			tc.mutate(&result)
			_, err := s.PutStepResult(t.Context(), result)
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "invalid field "+tc.field) || strings.Contains(err.Error(), "unknown-sensitive-value") {
				t.Fatalf("error=%v, want field %s without raw values", err, tc.field)
			}
		})
	}
}
