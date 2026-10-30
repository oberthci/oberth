package runlog

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

type waitingPatternReader struct {
	io.ReaderAt
	entered chan<- struct{}
	release <-chan struct{}
}

func (r waitingPatternReader) ReadAt(p []byte, off int64) (int, error) {
	r.entered <- struct{}{}
	<-r.release
	return r.ReaderAt.ReadAt(p, off)
}

func TestBoundedPatternConcurrencyWaitUsesRequestDeadline(t *testing.T) {
	raw := []byte("[t/u] match\n")
	entered, release := make(chan struct{}, 2), make(chan struct{})
	done := make(chan error, 2)
	// Both scans block in their actual reads while holding the two CPU slots.
	for range 2 {
		go func() {
			_, _, err := scanBoundedRanges(waitingPatternReader{bytes.NewReader(raw), entered, release}, []Range{{End: int64(len(raw))}}, int64(len(raw)), Filter{Pattern: "match"}.WithContext(t.Context()))
			done <- err
		}()
	}
	for range 2 {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatal("pattern scans did not start")
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	reader := &countedLogReader{ReaderAt: bytes.NewReader(raw)}
	_, _, err := scanBoundedRanges(reader, []Range{{End: int64(len(raw))}}, int64(len(raw)), Filter{Pattern: "match"}.WithContext(ctx))
	close(release)
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if !errors.Is(err, context.DeadlineExceeded) || reader.bytes != 0 {
		t.Fatalf("waiting scan ignored budget: %v I/O=%d", err, reader.bytes)
	}
	body, _, err := scanBoundedRanges(bytes.NewReader(raw), []Range{{End: int64(len(raw))}}, int64(len(raw)), Filter{Pattern: "match"}.WithContext(t.Context()))
	if err != nil || !bytes.Equal(body, raw) {
		t.Fatalf("pattern slot leaked: %q %v", body, err)
	}
}
