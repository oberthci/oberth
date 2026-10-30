package runlog

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/oberthci/oberth/internal/runprogress"
)

type countedLogReader struct {
	io.ReaderAt
	bytes int64
}

func (r *countedLogReader) ReadAt(p []byte, off int64) (int, error) {
	n, err := r.ReaderAt.ReadAt(p, off)
	r.bytes += int64(n)
	return n, err
}

func TestBoundedHeadStopsAtLimitAndReportsUnscannedData(t *testing.T) {
	line := "[t/u] match " + strings.Repeat("x", 100) + "\n"
	raw := []byte(strings.Repeat(line, 100000))
	reader := &countedLogReader{ReaderAt: bytes.NewReader(raw)}
	body, meta, err := scanBoundedRanges(reader, []Range{{End: int64(len(raw))}}, int64(len(raw)), Filter{Pattern: "match", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != line || meta.ReturnedLines != 1 || meta.ScanComplete || !meta.HasMore || !meta.BytesExact {
		t.Fatalf("unexpected bounded head: %s %+v", body, meta)
	}
	if reader.bytes > initialTailBytes || meta.ScannedBytes != reader.bytes || meta.NextByteOffset != int64(len(line)) {
		t.Fatalf("head read beyond its first buffer: I/O=%d meta=%+v", reader.bytes, meta)
	}
	if meta.WithheldBytes != int64(len(raw)-len(body)) || !meta.Truncated {
		t.Fatalf("withheld bytes were hidden: %+v", meta)
	}
}

func TestBoundedTailCapsSparsePatternWorkAndMovesBackward(t *testing.T) {
	raw := []byte(strings.Repeat("[t/u] noise\n", 1000000))
	var cursor *int64
	for range 2 {
		reader := &countedLogReader{ReaderAt: bytes.NewReader(raw)}
		body, meta, err := scanBoundedRanges(reader, []Range{{End: int64(len(raw))}}, int64(len(raw)), Filter{Pattern: "absent", Limit: 20, Tail: true, ByteOffset: cursor})
		if err != nil {
			t.Fatal(err)
		}
		if len(body) != 0 || !meta.HasMore || meta.ScanComplete || meta.ScannedBytes > MaxScanBytes || reader.bytes > MaxScanBytes {
			t.Fatalf("sparse pattern escaped scan budget: I/O=%d %+v", reader.bytes, meta)
		}
		before := int64(len(raw))
		if cursor != nil {
			before = *cursor
		}
		if meta.NextByteOffset >= before {
			t.Fatalf("tail cursor did not progress: before=%d %+v", before, meta)
		}
		cursor = &meta.NextByteOffset
	}
}

func TestBoundedByteCursorPagesWithoutLosingOrDuplicatingLines(t *testing.T) {
	var lines []string
	for i := range 1200 {
		lines = append(lines, fmt.Sprintf("[t/u] %04d 界 %s", i, strings.Repeat("x", 100)))
	}
	store := seedRun(t, "cursor", lines)
	var combined []byte
	var cursor *int64
	for page := 0; page < 200; page++ {
		body, meta, err := store.ReadFiltered("cursor", "t", "u", Filter{Pattern: "界", Limit: 7, ByteOffset: cursor})
		if err != nil {
			t.Fatal(err)
		}
		combined = append(combined, body...)
		if !meta.HasMore {
			break
		}
		if cursor != nil && meta.NextByteOffset <= *cursor {
			t.Fatal("head cursor did not progress")
		}
		cursor = &meta.NextByteOffset
	}
	if string(combined) != strings.Join(lines, "\n")+"\n" {
		t.Fatal("byte paging lost, duplicated or split a UTF-8 record")
	}
}

func TestBoundedTailCursorDoesNotSkipMatchesEvictedFromLookback(t *testing.T) {
	lines := numberedStep("[t/u]", 6000)
	store := seedRun(t, "tail-cursor", lines)
	var pages [][]byte
	var cursor *int64
	for page := 0; page < 1000; page++ {
		body, meta, err := store.ReadFiltered("tail-cursor", "t", "u", Filter{Pattern: "line", Tail: true, Limit: 7, ByteOffset: cursor})
		if err != nil {
			t.Fatal(err)
		}
		pages = append(pages, body)
		if !meta.HasMore {
			break
		}
		if cursor != nil && meta.NextByteOffset >= *cursor {
			t.Fatal("tail cursor did not progress")
		}
		cursor = &meta.NextByteOffset
	}
	var combined []byte
	for i := len(pages) - 1; i >= 0; i-- {
		combined = append(combined, pages[i]...)
	}
	if string(combined) != strings.Join(lines, "\n")+"\n" {
		t.Fatal("tail cursor skipped or duplicated matches inside a wider lookback window")
	}
}

func TestBoundedScanBoundaryPreservesWholeUTF8Records(t *testing.T) {
	line := "[t/u] " + strings.Repeat("界", 997) + "\n"
	raw := strings.Repeat(line, 2500)
	for _, active := range []bool{false, true} {
		t.Run(fmt.Sprint(active), func(t *testing.T) {
			store, err := Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			file, err := store.Create("boundary")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := file.WriteString(raw); err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			if !active {
				if _, err := store.BuildIndex("boundary"); err != nil {
					t.Fatal(err)
				}
			}
			var combined []byte
			var cursor *int64
			for range 5 {
				filter := Filter{ByteOffset: cursor}
				var body []byte
				var meta Meta
				var err error
				if active {
					body, meta, err = store.ReadActiveFiltered("boundary", "t", "u", filter)
				} else {
					body, meta, err = store.ReadFiltered("boundary", "t", "u", filter)
				}
				if err != nil {
					t.Fatal(err)
				}
				if !utf8.Valid(body) || meta.ScannedBytes > MaxScanBytes || meta.DiscoveryBytes > MaxScanBytes {
					t.Fatalf("record/budget boundary violated: %+v", meta)
				}
				combined = append(combined, body...)
				if !meta.HasMore {
					break
				}
				cursor = &meta.NextByteOffset
			}
			if string(combined) != raw {
				t.Fatalf("scan boundary lost a record: got %d bytes, want %d", len(combined), len(raw))
			}
		})
	}
}

func TestActiveBoundedDiscoveryDoesNotMistakeAnotherStepsTailForAbsence(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	file, err := store.Create("active-window")
	if err != nil {
		t.Fatal(err)
	}
	wanted := "[wanted/step] original output\n"
	if _, err := file.WriteString(wanted + strings.Repeat("[other/step] noise\n", 300000)); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	body, first, err := store.ReadActiveFiltered("active-window", "wanted", "step", Filter{Tail: true, Limit: 1})
	if err != nil || len(body) != 0 || !first.HasMore || first.ScanComplete || first.BytesExact || first.DiscoveryBytes > MaxScanBytes {
		t.Fatalf("partial discovery claimed absence/completeness: %q %+v %v", body, first, err)
	}
	body, second, err := store.ReadActiveFiltered("active-window", "wanted", "step", Filter{Tail: true, Limit: 1, ByteOffset: &first.NextByteOffset})
	if err != nil || string(body) != wanted || second.DiscoveryBytes > MaxScanBytes {
		t.Fatalf("older active page: %q %+v %v", body, second, err)
	}
	if _, err := os.Stat(store.directory + "/active-window.index.json"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("active request wrote a terminal index")
	}
}

func TestBoundedOversizedRecordsAreOmittedAndTailCursorStillProgresses(t *testing.T) {
	raw := []byte("[t/u] " + strings.Repeat("x", maxFilterLineBytes*2) + "\n[t/u] final\n")
	body, meta, err := scanBoundedRanges(bytes.NewReader(raw), []Range{{End: int64(len(raw))}}, int64(len(raw)), Filter{})
	if err != nil || string(body) != "[t/u] final\n" || meta.OmittedLongLines != 1 || meta.WithheldLines != 1 || !meta.Truncated {
		t.Fatalf("oversized record not bounded: %q %+v %v", body, meta, err)
	}
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	file, err := store.Create("one-long-record")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("[t/u] " + strings.Repeat("x", int(MaxScanBytes)*2)); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	_, meta, err = store.ReadActiveFiltered("one-long-record", "t", "u", Filter{Tail: true, Limit: 1})
	if err != nil || !meta.HasMore || meta.NextByteOffset >= MaxScanBytes*2 || meta.NextByteOffset < 0 {
		t.Fatalf("oversized active record trapped tail cursor: %+v %v", meta, err)
	}
}

func TestBoundedScanHonorsCancellationAndReadErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	reader := &countedLogReader{ReaderAt: bytes.NewReader([]byte("[t/u] data\n"))}
	if _, _, err := scanBoundedRanges(reader, []Range{{End: 11}}, 11, Filter{}.WithContext(ctx)); !errors.Is(err, context.Canceled) || reader.bytes != 0 {
		t.Fatalf("canceled read performed I/O: %v %d", err, reader.bytes)
	}
	if _, _, err := scanBoundedRanges(failingLogReader{}, []Range{{End: 11}}, 11, Filter{}); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("read failure swallowed as EOF: %v", err)
	}
}

type failingLogReader struct{}

func (failingLogReader) ReadAt([]byte, int64) (int, error) { return 0, os.ErrPermission }

func TestRunProgressReadDoesNotWaitForGlobalAppenderLock(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := store.AppendStepProgress("progress-lock", runprogress.Event{Version: runprogress.Version, Burn: "t", Step: "u", Status: runprogress.StepRunning, DeclaredSize: "M", StartedAt: &now}); err != nil {
		t.Fatal(err)
	}
	store.progressMu.Lock()
	defer store.progressMu.Unlock()
	done := make(chan error, 1)
	go func() { _, err := store.StepProgress("progress-lock"); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("run_get journal read blocked behind unrelated append")
	}
}
