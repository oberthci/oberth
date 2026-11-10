package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/oberthci/oberth/internal/store"
)

// RestoredRunsFile is the marker oberth restore leaves on the data volume
// beside the restored database: the IDs of runs the backup recorded as
// running. Their Workflows belonged to the backed-up installation; if none
// is found here, startup recovery marks such a run interrupted instead of
// requeueing it, so a restore never re-executes a pipeline (or re-burns a
// release) on its own.
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
	file, err := os.Open(path) // #nosec G304 -- fixed marker name on the server's own data volume.
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
