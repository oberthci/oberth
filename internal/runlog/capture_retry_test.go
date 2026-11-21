package runlog

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type singleReadSource struct {
	*strings.Reader
	bytes, closed int
}

func (s *singleReadSource) Read(p []byte) (int, error) {
	n, err := s.Reader.Read(p)
	s.bytes += n
	return n, err
}
func (s *singleReadSource) Close() error { s.closed++; return nil }
func TestCaptureRetriesStorageWithoutReopeningSource(t *testing.T) {
	s := newStore(t, 1024)
	r, err := s.Reserve(t.Context(), "run", native("one"))
	if err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(s.directory, "run.log")
	if err = os.Mkdir(blocked, 0700); err != nil {
		t.Fatal(err)
	}
	source := &singleReadSource{Reader: strings.NewReader("body\n")}
	attempts := 0
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	err = r.Capture(ctx, CaptureRequest{Source: source, Retry: func(ctx context.Context) error {
		attempts++
		if _, ok := ctx.Deadline(); !ok {
			t.Error("unbounded retry")
		}
		return os.Remove(blocked)
	}})
	if err != nil || attempts != 1 || source.bytes != 5 || source.closed != 1 {
		t.Fatal(err, attempts, source)
	}
	body, _, err := s.Read(t.Context(), "run", ReadRequest{})
	if err != nil || string(body) != "body\n" {
		t.Fatal(string(body), err)
	}
	if next, err := s.Reserve(t.Context(), "run", native("one")); err != nil || next != nil {
		t.Fatal("reopened physical receipt", next, err)
	}
}
func TestCaptureStorageRetryStopsAtContextAndLeavesInterruptedReceipt(t *testing.T) {
	s := newStore(t, 1024)
	r, err := s.Reserve(t.Context(), "run", native("one"))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(filepath.Join(s.directory, "run.log"), 0700); err != nil {
		t.Fatal(err)
	}
	source := &singleReadSource{Reader: strings.NewReader("body\n")}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	attempts := 0
	err = r.Capture(ctx, CaptureRequest{Source: source, Retry: func(ctx context.Context) error { attempts++; cancel(); return ctx.Err() }})
	if !errors.Is(err, context.Canceled) || attempts != 1 || source.closed != 1 {
		t.Fatal(err, attempts, source)
	}
	entries, err := s.Containers(t.Context(), "run")
	if err != nil || len(entries) != 1 || entries[0].State != Interrupted {
		t.Fatal("failed capture lost its receipt", entries, err)
	}
}
func TestCommitRetryDiscardsOnlyUnpublishedBytesAndAcceptsPublishedReceipt(t *testing.T) {
	s := newStore(t, 1024)
	if err := s.Append(t.Context(), "run", []byte("prefix\n")); err != nil {
		t.Fatal(err)
	}
	r, err := s.Reserve(t.Context(), "run", native("one"))
	if err != nil {
		t.Fatal(err)
	}
	spool, err := os.CreateTemp(t.TempDir(), "spool")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = spool.Close() }()
	if _, err = io.WriteString(spool, "body\n"); err != nil {
		t.Fatal(err)
	}
	appendUnpublished(t, s)
	for range 2 {
		if err = r.commit(t.Context(), spool, captureResult{size: 5}); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.Append(t.Context(), "run", []byte("suffix\n")); err != nil {
		t.Fatal(err)
	}
	if err = r.commit(t.Context(), spool, captureResult{size: 5}); err != nil {
		t.Fatal(err)
	}
	body, _, err := s.Read(t.Context(), "run", ReadRequest{})
	if err != nil || string(body) != "prefix\nbody\nsuffix\n" {
		t.Fatal("commit retry duplicated or lost output", string(body), err)
	}
}

func TestCaptureRetriesStagingBeforeReadingSource(t *testing.T) {
	s := newStore(t, 1024)
	r, err := s.Reserve(t.Context(), "run", native("one"))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.root.Close(); err != nil {
		t.Fatal(err)
	}
	source := &singleReadSource{Reader: strings.NewReader("body\n")}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	retries := 0
	err = r.Capture(ctx, CaptureRequest{Source: source, Retry: func(context.Context) error {
		retries++
		if source.bytes != 0 {
			t.Error("read before staging")
		}
		var err error
		s.root, err = os.OpenRoot(s.directory)
		return err
	}})
	if err != nil || retries != 1 || source.bytes != 5 || source.closed != 1 {
		t.Fatal(err, retries, source)
	}
}

func appendUnpublished(t *testing.T, s *Store) {
	t.Helper()
	file, err := os.OpenFile(filepath.Join(s.directory, "run.log"), os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.WriteString(file, "unpublished tail"); err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
}
