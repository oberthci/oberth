package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

// RestoredRunsFile is the marker oberth restore leaves on the data volume
// beside the restored database: the IDs of runs the backup recorded as
// running, and for a rollback restore also those queued or waiting. A
// running run whose Workflow is found here is adopted; otherwise startup
// recovery marks it interrupted instead of requeueing it. Queued, waiting
// and mid-submission restored runs are cancelled (interrupted, with
// restoredRunCancelReason), because they may already have run on the
// version the rollback undid. A restore never re-executes a pipeline (or
// re-burns a release) on its own.
const RestoredRunsFile = ".oberth-restored-runs.json"

// errRestoredRun keeps a restored run out of the stranded-run requeue.
var errRestoredRun = fmt.Errorf("%w: run was running when the restored backup was taken", store.ErrRequeueIneligible)

const restoredRunInterruption = "run was running when the restored backup was taken; restored without re-executing it"

// loadRestoredRuns reads the marker; a missing file is an empty set. An
// unreadable or malformed marker is refused rather than ignored, because
// ignoring it would re-execute the runs it names.
func loadRestoredRuns(path string) (map[string]bool, error) {
	if path == "" {
		return nil, nil
	}
	// The marker has a fixed name in the data root; os.Root keeps the open
	// inside that directory.
	root, err := os.OpenRoot(filepath.Dir(path))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read restored-runs marker %s: %w", path, err)
	}
	defer func() { _ = root.Close() }()
	file, err := root.Open(filepath.Base(path))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read restored-runs marker %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()
	raw, err := io.ReadAll(io.LimitReader(file, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("read restored-runs marker %s: %w", path, err)
	}
	var marker struct {
		Runs []string `json:"runs"`
	}
	if err := json.Unmarshal(raw, &marker); err != nil {
		return nil, fmt.Errorf("decode restored-runs marker %s: %w", path, err)
	}
	set := make(map[string]bool, len(marker.Runs))
	for _, id := range marker.Runs {
		set[id] = true
	}
	return set, nil
}

// retireRestoredRunsMarker removes the marker once startup reconciliation
// has handled every run it names.
func retireRestoredRunsMarker(path string) error {
	if path == "" {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove restored-runs marker %s: %w", path, err)
	}
	return nil
}

// restoredRunCancelReason is the reason line of a cancelled restored run.
const restoredRunCancelReason = "restored from a backup; not dispatched again"

// cancelRestoredRun cancels one restored active run through the ordinary
// cancel path (interrupted, phase cancelled, audited, Workflow-deletion
// obligation for a claimed run). A run already terminal or absent is left.
func (scheduler *Scheduler) cancelRestoredRun(ctx context.Context, id string, require mutationRequirement) error {
	canceller, ok := scheduler.store.(interface {
		CancelRun(context.Context, model.RunCancelRequest) (model.RunCancelResult, error)
	})
	if !ok {
		return fmt.Errorf("restored run %s cannot be cancelled by this store; refusing to dispatch it again", id)
	}
	if err := require(ctx); err != nil {
		return err
	}
	_, err := canceller.CancelRun(ctx, model.RunCancelRequest{RunID: id, Actor: "restore", Reason: restoredRunCancelReason})
	switch {
	case err == nil:
		scheduler.signals.NotifyRun(id)
		return nil
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrRunNotActive):
		return nil
	}
	return fmt.Errorf("cancel restored run %s: %w", id, err)
}
