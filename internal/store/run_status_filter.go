package store

import (
	"database/sql/driver"
	"fmt"
	"path"
	"strings"
	"time"

	"modernc.org/sqlite"

	"github.com/oberthci/oberth/internal/model"
)

// Use the presentation classifier rather than a second SQL implementation of
// deadlines, pending creates and retry lineage. Registered once before any
// connection opens; no schema objects depend on this read-only function.
func init() {
	sqlite.MustRegisterDeterministicScalarFunction("oberth_run_display_status", 5, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		values := make([]string, 4)
		for i := range values {
			value, ok := args[i].(string)
			if !ok {
				return nil, fmt.Errorf("invalid run display column %d", i)
			}
			values[i] = value
		}
		run := model.Run{Status: model.RunStatus(values[0]), Phase: values[1], Reason: values[2], Error: values[3]}
		if started, ok := args[4].(int64); ok && started != 0 {
			at := time.Unix(0, started)
			run.StartedAt = &at
		}

		return model.DisplayRunStatus(run), nil
	})
	// The run list's ref_glob (#1524) uses the same path.Match semantics as
	// the agent events' refs_glob: * never crosses a slash.
	sqlite.MustRegisterDeterministicScalarFunction("oberth_ref_glob", 2, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		pattern, ok := args[0].(string)
		ref, refOK := args[1].(string)
		if !ok || !refOK {
			return int64(0), nil
		}
		matched, err := path.Match(pattern, ref)
		if err != nil || !matched {
			return int64(0), nil
		}
		return int64(1), nil
	})
}

func runStatusRestriction(statuses model.RunStatusFilter) (string, []any, error) {
	if err := statuses.Validate(); err != nil {
		return "", nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if len(statuses) == 0 {
		return "", nil, nil
	}
	// Narrow by durable status first: a queued-only query must not scan all
	// retained terminal rows just to evaluate the display classifier.
	durable := map[string]bool{}
	for _, status := range statuses {
		switch status {
		case "queued":
			durable["queued"], durable["running"] = true, true
		case "running":
			durable["running"] = true
		case "failed":
			durable["failed"] = true
		case "cancelled":
			durable["interrupted"] = true
		case "passed":
			durable["passed"] = true
		}
	}
	args := make([]any, 0, len(durable)+len(statuses))
	for _, status := range []string{"queued", "running", "passed", "failed", "interrupted"} {
		if durable[status] {
			args = append(args, status)
		}
	}
	coarse := strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")
	for _, status := range statuses {
		args = append(args, status)
	}
	return ` AND runs.status IN (` + coarse + `) AND oberth_run_display_status(runs.status,runs.phase,runs.reason,runs.error,runs.started_at IS NOT NULL) IN (` + strings.TrimSuffix(strings.Repeat("?,", len(statuses)), ",") + `)`, args, nil
}
