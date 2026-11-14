package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/store"
)

func logStepSelector(row model.StepResult) string {
	if row.NodeID != "" {
		return row.NodeID
	}
	return row.Step
}
func loggableNode(row model.StepResult) bool {
	switch row.NodeType {
	case "", "Pod", "Container", "Skipped":
		return true
	default:
		return false
	}
}

// A native ID always selects exactly one row. Display names are only a
// discovery convenience and must resolve uniquely before any bytes are read.
func selectLogNode(rows []model.StepResult, stage, wanted string) (model.StepResult, error) {
	if wanted != "" {
		for _, row := range rows {
			if row.NodeID == wanted {
				return row, nil
			}
		}
	}
	var matches []model.StepResult
	for _, row := range rows {
		if (stage == "" || row.Stage == stage) && (wanted == "" || row.Step == wanted || row.DisplayName == wanted) && loggableNode(row) {
			matches = append(matches, row)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) == 0 {
		return model.StepResult{}, fmt.Errorf("no log range for burn %q step %q: %w", stage, wanted, store.ErrNotFound)
	}
	var ids, stages []string
	seen := map[string]bool{}
	for _, row := range matches {
		ids = append(ids, logStepSelector(row))
		if !seen[row.Stage] {
			stages = append(stages, row.Stage)
			seen[row.Stage] = true
		}
	}
	sort.Strings(ids)
	sort.Strings(stages)
	if wanted == "" {
		return model.StepResult{}, fmt.Errorf("%w: step is required unless burn %q has exactly one loggable node; choose Argo node ID (%s)", ErrInvalidInput, stage, strings.Join(ids, ", "))
	}
	return model.StepResult{}, fmt.Errorf("%w: step %q matches burns: %s; specify burn or an Argo node ID (%s)", ErrInvalidInput, wanted, strings.Join(stages, ", "), strings.Join(ids, ", "))
}

func (service *API) readSelectedNodeLog(ctx context.Context, run model.Run, row model.StepResult, filter runlog.Filter) ([]byte, runlog.Meta, error) {
	if err := service.purgedEvidence(ctx, run, "logs"); err != nil {
		return nil, runlog.Meta{}, err
	}
	entries, err := service.logs.ContainerLogs(ctx, run.ID)
	if err != nil {
		return nil, runlog.Meta{}, err
	}
	native := row.NodeType != ""
	var selected *runlog.ContainerLog
	for i := range entries {
		entry := &entries[i]
		if entry.NodeID != row.NodeID {
			continue
		}
		native = true
		if entry.Main || entry.ContainerName == "main" || entry.ContainerName == "" {
			selected = entry
			break
		}
	}
	if !native {
		return service.readStepLogFiltered(ctx, run, row.Stage, row.Step, filter)
	}
	if selected == nil {
		if run.Status.Active() {
			return nil, runlog.Meta{}, fmt.Errorf("%w: native node %s has no retained output yet; request its live container stream", ErrStepLogPending, row.NodeID)
		}
		return nil, runlog.Meta{}, fmt.Errorf("native node %s has no retained container log: %w", row.NodeID, os.ErrNotExist)
	}
	if filter.Pattern != "" {
		release, err := runLogPatternScans.acquire(ctx)
		if err != nil {
			return nil, runlog.Meta{}, err
		}
		defer release()
	}
	body, meta, _, err := service.logs.ReadContainerLog(ctx, run.ID, row.NodeID, selected.ContainerName, filter.WithContext(ctx))
	if errors.Is(err, os.ErrNotExist) && run.Status.Active() {
		return nil, meta, fmt.Errorf("%w: native container copy is pending; request its live stream", ErrStepLogPending)
	}
	return body, meta, err
}
