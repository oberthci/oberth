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
	if err := s.room(t.Context()); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "source.tgz")
	if err := os.WriteFile(file, []byte("archive"), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	return s.put([sha256.Size]byte{key}, dir, sourcedelivery.Staged{Body: f, Bytes: 7})
}

func TestStagedArchivesBoundReadersAndEvictIdle(t *testing.T) {
	t.Parallel()
	s := newStagedArchives()
	first, second := addTestArchive(t, s, 1), addTestArchive(t, s, 2)
	t.Cleanup(func() { _ = first.Body.Close(); _ = second.Body.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := s.room(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("active files exceeded scratch bound", err)
	}
	if err := first.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if err := first.Body.Close(); err != nil {
		t.Fatal("duplicate Close", err)
	}
	third := addTestArchive(t, s, 3)
	defer func() { _ = third.Body.Close() }()
	if _, found, _ := s.open([sha256.Size]byte{1}); found {
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
	s := newStagedArchives()
	first, second := addTestArchive(t, s, 1), addTestArchive(t, s, 2)
	defer func() { _ = first.Body.Close(); _ = second.Body.Close() }()
	done := make(chan error, 1)
	go func() { done <- s.room(t.Context()) }()
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
