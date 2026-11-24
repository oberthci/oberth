package runlog

import (
	"context"
	"io"
	"strings"
	"testing"
)

func TestFinalArchiveRetriesOnlyUnpublishedBytesAfterRestart(t *testing.T) {
	directory := t.TempDir()
	s, err := Open(t.Context(), directory, 1024)
	if err != nil {
		t.Fatal(err)
	}
	entry := native("one")
	request := SourceCapture{RunID: "run", Container: entry, Open: func(context.Context) (io.ReadCloser, error) {
		return io.NopCloser(io.MultiReader(strings.NewReader("partial"), &brokenSource{})), nil
	}}
	if err = Archive(t.Context(), s, request); err == nil {
		t.Fatal("incomplete archive was published")
	}
	if ready, err := s.Archived(t.Context(), "run", []ContainerLog{entry}); err != nil || ready {
		t.Fatal("incomplete archive permits cleanup", ready, err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(t.Context(), directory, 1024)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	opens := 0
	request.Open = func(context.Context) (io.ReadCloser, error) {
		opens++
		return io.NopCloser(strings.NewReader("complete output\n")), nil
	}
	for range 2 {
		if err = Archive(t.Context(), s, request); err != nil {
			t.Fatal(err)
		}
	}
	assertFinalArchive(t, s, entry, opens)
}

func TestFinalArchiveBoundsAndChecksItsDurableBody(t *testing.T) {
	s := newStore(t, 8)
	entry := native("one")
	request := SourceCapture{RunID: "run", Container: entry, Open: func(context.Context) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("bounded output exceeds the limit")), nil
	}}
	if err := Archive(t.Context(), s, request); err != nil {
		t.Fatal(err)
	}
	if ready, err := s.Archived(t.Context(), "run", []ContainerLog{entry}); err != nil || !ready {
		t.Fatal("bounded archive not collectible", ready, err)
	}
	body, meta, err := s.Read(t.Context(), "run", ReadRequest{})
	if err != nil || string(body) != "bounded " || meta.Reason != limitReason {
		t.Fatal("archive cap lost", string(body), meta, err)
	}
	if err := s.root.Remove("run.log"); err != nil {
		t.Fatal(err)
	}
	if ready, err := s.Archived(t.Context(), "run", []ContainerLog{entry}); err == nil || ready {
		t.Fatal("missing durable bytes permit cleanup", ready, err)
	}
}

func assertFinalArchive(t *testing.T, s *Store, entry ContainerLog, opens int) {
	t.Helper()
	body, _, err := s.Read(t.Context(), "run", ReadRequest{})
	ready, check := s.Archived(t.Context(), "run", []ContainerLog{entry})
	if err != nil || check != nil || !ready {
		t.Fatal("archive is not durable", ready, err, check)
	}
	if opens != 1 || string(body) != "complete output\n" {
		t.Fatal("archive not durable or completed source reopened", string(body), opens, ready, err, check)
	}
}
