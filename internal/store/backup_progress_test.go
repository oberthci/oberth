package store

import (
	"io"
	"testing"

	"modernc.org/sqlite"
)

func TestSnapshotProgressTracksCompletedPageBatches(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	b, err := NewBackup(s.path)
	requireOK(t, err)
	steps, observations := 0, 0
	config := defaultSnapshot(1 << 30)
	config.step = func(copy *sqlite.Backup) (bool, error) { steps++; return copy.Step(1) }
	b.snapshot = &config
	b.Progress = func() { observations++ }
	_, err = b.Snapshot(t.Context(), backupOptions(s), io.Discard)
	requireOK(t, err)
	if observations == 0 || observations != steps {
		t.Fatal("snapshot work did not report progress", steps, observations)
	}
}
