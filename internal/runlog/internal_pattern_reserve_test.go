package runlog

import (
	"bytes"
	"context"
	"io"
	"sync"
	"testing"
	"time"
)

type pausedInternalPatternReader struct {
	io.ReaderAt
	entered chan<- struct{}
	release <-chan struct{}
}

func (r pausedInternalPatternReader) ReadAt(p []byte, off int64) (int, error) {
	r.entered <- struct{}{}
	<-r.release
	return r.ReaderAt.ReadAt(p, off)
}

func TestInternalPatternReadDoesNotWaitForOtherPatternScans(t *testing.T) {
	raw := []byte("[t/u] Error: CI diagnostic\n")
	entered, unblock := make(chan struct{}, 2), make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(unblock) }) }
	defer release()
	done := make(chan error, 2)
	for range 2 {
		go func() {
			_, _, err := scanBoundedRanges(pausedInternalPatternReader{bytes.NewReader(raw), entered, unblock}, []Range{{End: int64(len(raw))}}, int64(len(raw)), Filter{Pattern: "Error:"}.WithContext(t.Context()))
			done <- err
		}()
	}
	for range 2 {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatal("pattern reads did not start")
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	body, meta, err := scanBoundedRanges(bytes.NewReader(raw), []Range{{End: int64(len(raw))}}, int64(len(raw)), Filter{Pattern: "Error:"}.WithContext(ctx))
	release()
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if err != nil || !bytes.Equal(body, raw) || meta.ScannedBytes > MaxScanBytes {
		t.Fatalf("CI/internal read waited behind unrelated pattern scans: body=%q meta=%+v err=%v", body, meta, err)
	}
}
