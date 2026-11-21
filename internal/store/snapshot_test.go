package store

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"modernc.org/sqlite"
)

func TestSnapshotPinsCommittedWALAcrossBackupSteps(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	const initialIssues = 12
	for range initialIssues {
		_, err := s.CreateIssue(t.Context(), stateport.Action{Actor: "operator"}, model.Issue{Kind: model.IssueManual, Title: "first", Body: strings.Repeat("x", 96<<10)})
		requireOK(t, err)
	}
	before := independentHead(t, s.readers)
	info, err := os.Stat(s.path + "-wal")
	requireOK(t, err)
	if info.Size() == 0 {
		t.Fatal("fixture has no committed WAL")
	}
	config := defaultSnapshot(stateport.DefaultMaxBytes)
	steps := 0
	config.step = func(backup *sqlite.Backup) (bool, error) {
		more, err := backup.Step(128)
		steps++
		if steps == 1 {
			_, writeErr := s.CreateIssue(t.Context(), stateport.Action{Actor: "operator"}, model.Issue{Kind: model.IssueManual, Title: "later"})
			requireOK(t, writeErr)
		}
		return more, err
	}
	destination := filepath.Join(t.TempDir(), "snapshot.sqlite")
	requireOK(t, snapshotWith(t.Context(), s.path, destination, config))
	report, err := verifyDatabase(t.Context(), destination, backupOptions(s))
	requireOK(t, err)
	if steps < 2 || report.AuditHead != before {
		t.Fatal("snapshot did not pin original committed view", steps, report)
	}
	db := pool(destination, "ro", s.options, 1)
	defer func() { requireOK(t, db.Close()) }()
	var count int
	requireOK(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM issues`).Scan(&count))
	if count != initialIssues {
		t.Fatal("snapshot mixed revisions", count)
	}
}

func TestSnapshotSpaceAtLimitAndOneByteShort(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	size := snapshotDatabaseSize(t, s)
	needed := size + max(int64(256<<20), size/4) + (64 << 10)
	config := defaultSnapshot(stateport.DefaultMaxBytes)
	config.free = func(string) (int64, error) { return needed, nil }
	b := &Backup{path: s.path, snapshot: &config}
	_, err := b.Snapshot(t.Context(), backupOptions(s), io.Discard)
	requireOK(t, err)
	config.free = func(string) (int64, error) { return needed - 1, nil }
	_, err = b.Snapshot(t.Context(), backupOptions(s), io.Discard)
	requireCode(t, err, publicerr.Unavailable)
	var refusal *publicerr.Error
	if !errors.As(err, &refusal) || !strings.Contains(refusal.Detail, "Available") || !strings.Contains(refusal.Detail, "required") {
		t.Fatal("measured space missing", err)
	}
}

func TestRestoreSyncFailuresAndNoReplacement(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	var body bytes.Buffer
	_, err := s.Snapshot(t.Context(), backupOptions(s), &body)
	requireOK(t, err)
	cause := errors.New("sync failed")
	b := &Backup{path: s.path, syncFile: func(string) error { return cause }}
	request := stateport.RestoreRequest{Options: backupOptions(s), Destination: filepath.Join(t.TempDir(), "restore.sqlite")}
	_, err = b.Restore(t.Context(), request, bytes.NewReader(body.Bytes()))
	if !errors.Is(err, cause) {
		t.Fatal("sync cause lost", err)
	}
	if _, err := os.Stat(request.Destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("prepublication failure left destination", err)
	}
	b.syncFile = nil
	b.syncParent = func(string) error { return cause }
	_, err = b.Restore(t.Context(), request, bytes.NewReader(body.Bytes()))
	if !errors.Is(err, cause) {
		t.Fatal("parent sync cause lost", err)
	}
	// Publication can leave a complete file; failure never authorizes replay.
	_, err = verifyDatabase(t.Context(), request.Destination, request.Options)
	requireOK(t, err)
	_, err = b.Restore(t.Context(), request, bytes.NewReader(body.Bytes()))
	requireCode(t, err, publicerr.Conflict)
}

func TestStagingSweepKeepsActiveAndUnownedDirectories(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	stage, cleanup, err := newStage(parent)
	requireOK(t, err)
	requireOK(t, sweepStaging(parent))
	if _, err := os.Stat(stage); err != nil {
		t.Fatal("active staging swept", err)
	}
	requireOK(t, cleanup())
	stale := filepath.Join(parent, stagePrefix+"stale")
	requireOK(t, os.Mkdir(stale, 0700))
	requireOK(t, os.WriteFile(filepath.Join(stale, "owner"), []byte("oberth.state-stage/v1\n"), 0600))
	unowned := filepath.Join(parent, stagePrefix+"unowned")
	requireOK(t, os.Mkdir(unowned, 0700))
	requireOK(t, sweepStaging(parent))
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("owned abandoned staging retained", err)
	}
	if _, err := os.Stat(unowned); err != nil {
		t.Fatal("unowned directory swept", err)
	}
}

func TestSnapshotParentCompetitionAndSparseAllocation(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	held, err := lockParent(filepath.Dir(s.path))
	requireOK(t, err)
	_, err = s.Snapshot(t.Context(), backupOptions(s), io.Discard)
	requireCode(t, err, publicerr.Conflict)
	requireOK(t, held.Close())
	path := filepath.Join(t.TempDir(), "sparse.sqlite")
	file, err := os.Create(path)
	requireOK(t, err)
	size := int64(1 << 20)
	requireOK(t, file.Truncate(size))
	requireOK(t, file.Close())
	reserve := max(int64(256<<20), size/4) + (64 << 10)
	guard := spaceGuard{path: path, parent: filepath.Dir(path), size: size, free: func(string) (int64, error) { return reserve, nil }}
	requireCode(t, guard.check(), publicerr.Unavailable)
}

func snapshotDatabase(ctx context.Context, source, destination string, limit int64) error {
	return snapshotWith(ctx, source, destination, defaultSnapshot(limit))
}
