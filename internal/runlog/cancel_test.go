package runlog

import (
	"context"
	"errors"
	"io"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

type pausedSource struct {
	started chan struct{}
	closed  chan struct{}
	once    sync.Once
	closes  atomic.Int32
}

func (r *pausedSource) Read([]byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	<-r.closed
	return 0, io.EOF
}
func (r *pausedSource) Close() error {
	if r.closes.Add(1) == 1 {
		close(r.closed)
	}
	return nil
}
func TestSlowCaptureDoesNotHoldStoreGateAndCancellationJoins(t *testing.T) {
	s := newStore(t, 4096)
	entry := native("one")
	r, err := s.Reserve(t.Context(), "run", entry)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	source := &pausedSource{started: make(chan struct{}), closed: make(chan struct{})}
	finished := make(chan error, 1)
	go func() { finished <- r.Capture(ctx, CaptureRequest{Source: source, Gap: Gap{}}) }()
	<-source.started
	if err = s.Append(t.Context(), "run", []byte("observer remains live\n")); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Containers(t.Context(), "run"); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err = <-finished; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if source.closes.Load() != 1 {
		t.Fatal("source not closed once", source.closes.Load())
	}
	if err = r.Release(t.Context()); err == nil {
		t.Fatal("attempted copy released")
	}
	again, err := s.Reserve(t.Context(), "run", entry)
	if err != nil || again != nil {
		t.Fatal("interrupted source retried", err)
	}
}
func TestReleaseOnlyUnusedReservationAndCaptureFailureReason(t *testing.T) {
	s := newStore(t, 1024)
	entry := native("one")
	r, err := s.Reserve(t.Context(), "run", entry)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Release(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = r.Release(t.Context()); err == nil {
		t.Fatal("release repeated")
	}
	r, err = s.Reserve(t.Context(), "run", entry)
	if err != nil || r == nil {
		t.Fatal(err)
	}
	if err = r.Capture(t.Context(), CaptureRequest{Source: io.NopCloser(&brokenSource{}), Gap: Gap{}}); err != nil {
		t.Fatal(err)
	}
	values, err := s.Containers(t.Context(), "run")
	if err != nil || values[0].State != Captured || !strings.Contains(values[0].Reason, "interrupted") {
		t.Fatal(values, err)
	}
}

type brokenSource struct{}

func (*brokenSource) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func TestStoredStreamPlainBytesAndCancellation(t *testing.T) {
	s := newStore(t, 1024)
	body := "@@arbitrary@@ {\"status\":\"failed\"}\nplain\n"
	capture(t, s, native("one"), body)
	ctx, cancel := context.WithCancel(t.Context())
	stream, err := s.OpenStream(ctx, "run", ReadRequest{Selector: Selector{Step: "build"}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	got, err := io.ReadAll(stream)
	if err != nil || string(got) != body {
		t.Fatal(string(got), err)
	}
	cancel()
	if _, err = stream.Read(make([]byte, 1)); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

type generatedSource struct {
	remaining int64
	largest   int
	closed    bool
}

func (r *generatedSource) Read(body []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	r.largest = max(r.largest, len(body))
	n := min(int64(len(body)), r.remaining)
	for i := range body[:n] {
		body[i] = 'x'
	}
	r.remaining -= n
	return int(n), nil
}
func (r *generatedSource) Close() error { r.closed = true; return nil }
func TestLargeCaptureUsesBoundedStreamingMemory(t *testing.T) {
	s := newStore(t, 8<<20)
	r, err := s.Reserve(t.Context(), "run", native("one"))
	if err != nil {
		t.Fatal(err)
	}
	source := &generatedSource{remaining: 16 << 20}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if err = r.Capture(t.Context(), CaptureRequest{Source: source, Gap: Gap{}}); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 2<<20 {
		t.Fatal("capture buffered output in memory", allocated)
	}
	if source.largest > 32<<10 || !source.closed {
		t.Fatal("source read unbounded or not closed", source.largest)
	}
	entries, err := s.Containers(t.Context(), "run")
	if err != nil || entries[0].End > 8<<20 || entries[0].Reason == "" {
		t.Fatal(entries, err)
	}
}
