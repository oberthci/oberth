package gitcache

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/sourcedelivery"
)

func addTestArchive(t *testing.T, s *stagedArchives, key byte) sourcedelivery.Staged {
	t.Helper()
	e, leader, _, err := s.reserve(t.Context(), [sha256.Size]byte{key})
	if err != nil || !leader {
		t.Fatal("reservation", err)
	}
	defer s.leave(e)
	s.start(e, func() {}, func() {})
	dir := t.TempDir()
	file := filepath.Join(dir, "source.tgz")
	if err := os.WriteFile(file, []byte("archive"), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	s.complete(e, dir, sourcedelivery.Staged{Body: f, Bytes: 7}, nil)
	stage, err := s.open(e)
	if err != nil {
		t.Fatal(err)
	}
	return stage
}

func TestStagedArchivesBoundReadersAndEvictIdle(t *testing.T) {
	t.Parallel()
	s := newStagedArchives(2)
	first, second := addTestArchive(t, s, 1), addTestArchive(t, s, 2)
	t.Cleanup(func() { _ = first.Body.Close(); _ = second.Body.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, _, waited, err := s.reserve(ctx, [sha256.Size]byte{3}); !waited || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("active files exceeded scratch bound", waited, err)
	}
	if err := first.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if err := first.Body.Close(); err != nil {
		t.Fatal("duplicate Close", err)
	}
	third := addTestArchive(t, s, 3)
	defer func() { _ = third.Body.Close() }()
	if s.entries[[sha256.Size]byte{1}] != nil {
		t.Fatal("idle archive was not evicted")
	}
	if len(s.entries) != 2 {
		t.Fatal("archive capacity", len(s.entries))
	}
	if err := s.prune(nil); err != nil {
		t.Fatal(err)
	}
	if data, err := io.ReadAll(second.Body); err != nil || string(data) != "archive" {
		t.Fatal("GC changed an active snapshot", string(data), err)
	}
	_ = second.Body.Close()
	_ = third.Body.Close()
	if err := s.prune(nil); err != nil || len(s.entries) != 0 {
		t.Fatal("finished snapshots retained", err)
	}
}

func TestStagedArchiveRoomWakesOnReaderClose(t *testing.T) {
	t.Parallel()
	s := newStagedArchives(2)
	first, second := addTestArchive(t, s, 1), addTestArchive(t, s, 2)
	defer func() { _ = first.Body.Close(); _ = second.Body.Close() }()
	done := make(chan error, 1)
	go func() {
		e, leader, _, err := s.reserve(t.Context(), [sha256.Size]byte{3})
		if err == nil {
			s.start(e, func() {}, func() {})
			s.leave(e)
			if leader {
				s.complete(e, "", sourcedelivery.Staged{}, context.Canceled)
			}
		}
		done <- err
	}()
	if err := first.Body.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("archive slot did not wake after Close")
	}
}
