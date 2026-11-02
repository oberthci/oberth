package service

import (
	"context"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

type ResourceCoverageReader interface {
	ResourceCoverages(context.Context, []string) (map[string]model.ResourceCoverage, error)
}

// Call only for already-authorized runs. Like the resource-history reader, this
// dependency is raw; scoped repo/run wrappers establish access before using it.
// Chunking bounds SQL and avoids per-run reads on dashboard lists.
func (s *API) applyResourceMessages(ctx context.Context, runs []model.Run) {
	if s.resourceCoverages == nil || len(runs) == 0 {
		return
	}
	for start := 0; start < len(runs); start += 512 {
		end := min(start+512, len(runs))
		ids := make([]string, 0, end-start)
		for _, run := range runs[start:end] {
			ids = append(ids, run.ID)
		}
		coverage, err := s.resourceCoverages.ResourceCoverages(ctx, ids)
		if err != nil {
			continue
		}
		for i := start; i < end; i++ {
			message := coverage[runs[i].ID].Message(runs[i].ID)
			if message == "" {
				continue
			}
			if strings.Contains(runs[i].DisplayReason, message) {
				continue
			}
			if runs[i].DisplayReason == "" {
				runs[i].DisplayReason = runs[i].Error
			}
			runs[i].DisplayReason = strings.TrimSpace(runs[i].DisplayReason + " " + message)
		}
	}
}

// Failure guidance is durable in the existing error field, so list, detail and
// restart reconciliation agree without polling the execution engine per row.
func (s *Scheduler) withResourceFailure(ctx context.Context, runID string, result JobResult) JobResult {
	if result.Status != model.RunFailed {
		return result
	}
	deadline := model.IsExecutionDeadlineReason(result.Error)
	if model.ResourceFailureMessage(runID, result.Steps, nil, deadline) == "" {
		return result
	}
	var values []model.StepResourceUsage
	if reader, ok := s.store.(interface {
		StepResourceUsage(context.Context, string) ([]model.StepResourceUsage, bool, error)
	}); ok {
		readCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		values, _, _ = reader.StepResourceUsage(readCtx, runID)
	}
	if message := model.ResourceFailureMessage(runID, result.Steps, values, deadline); message != "" {
		result.Error = message
	}
	return result
}
