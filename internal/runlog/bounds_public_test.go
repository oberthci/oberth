package runlog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPublicFilterEdges(t *testing.T) {
	s := newStore(t, 2048)
	if err := s.Append(t.Context(), "run", []byte(strings.Repeat("a", 512)+"\n")); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		good, bad Filter
	}{
		{"context", Filter{Context: 50}, Filter{Context: 51}},
		{"context-lower", Filter{}, Filter{Context: -1}},
		{"byte-offset", Filter{ByteOffset: new(int64(0))}, Filter{ByteOffset: new(int64(-1))}},
		{"syntax", Filter{Pattern: "a"}, Filter{Pattern: "["}},
		{"pattern", Filter{Pattern: strings.Repeat("a", 512)}, Filter{Pattern: strings.Repeat("a", 513)}},
		{"offset", Filter{Offset: 0}, Filter{Offset: -1}},
		{"limit", Filter{Limit: 0}, Filter{Limit: -1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := s.Read(t.Context(), "run", ReadRequest{Selector: Selector{}, Filter: tc.good}); err != nil {
				t.Fatal("N refused", err)
			}
			if _, _, err := s.Read(t.Context(), "run", ReadRequest{Selector: Selector{}, Filter: tc.bad}); err == nil {
				t.Fatal("outside bound accepted")
			}
		})
	}
}
func TestPublicBindingEdges(t *testing.T) {
	for field := range 5 {
		for _, value := range []string{strings.Repeat("x", 1024), strings.Repeat("x", 1025), "", "x\r", "x\n", "x\x00"} {
			s := newStore(t, 1024)
			entry := native("one")
			fields := []*string{&entry.NodeID, &entry.PodUID, &entry.PodName, &entry.Namespace, &entry.Container}
			*fields[field] = value
			err := s.Track(t.Context(), "run", []ContainerLog{entry})
			if (len(value) == 1024) != (err == nil) {
				t.Fatalf("field %d, bytes %d: %v", field, len(value), err)
			}
		}
	}
}
func TestPublicRunIDAndPositiveLimitEdges(t *testing.T) {
	for _, n := range []int{80, 81} {
		s := newStore(t, 1)
		err := s.Track(t.Context(), strings.Repeat("x", n), []ContainerLog{native("one")})
		if (n == 80) != (err == nil) {
			t.Fatal(n, err)
		}
	}
	if _, err := Open(t.Context(), t.TempDir(), 0); err == nil {
		t.Fatal("zero limit accepted")
	}
	s := newStore(t, 1)
	if err := s.Append(t.Context(), "run", []byte("xy")); err != nil {
		t.Fatal(err)
	}
	body, meta, err := s.Read(t.Context(), "run", ReadRequest{Selector: Selector{}, Filter: Filter{}})
	if err != nil || string(body) != "x" || meta.Reason != "run log byte limit reached" {
		t.Fatal(meta, err)
	}
}
func TestPublicIndexReadAtEightMiB(t *testing.T) {
	s := newStore(t, 10)
	if err := s.Append(t.Context(), "run", []byte("x\n")); err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"run_id":"run","size":2,"containers":[]}`)
	raw = append(raw, bytes.Repeat([]byte(" "), (8<<20)-len(raw))...)
	for _, extra := range []bool{false, true} {
		data := raw
		if extra {
			data = append(bytes.Clone(raw), ' ')
		}
		if err := os.WriteFile(filepath.Join(s.directory, "run.index.json"), data, 0600); err != nil {
			t.Fatal(err)
		}
		_, _, err := s.Read(t.Context(), "run", ReadRequest{Selector: Selector{}, Filter: Filter{}})
		if extra != (err != nil) {
			t.Fatal("index read bound", extra, err)
		}
		usage, err := s.Usage(t.Context(), "run", usageClock())
		if err != nil || usage.AccountingComplete == extra || usage.Incomplete != extra {
			t.Fatal("usage index read bound", extra, usage, err)
		}
	}
}
func TestPublicIndexPublicationAtEightMiB(t *testing.T) {
	base := newStore(t, 1)
	r, err := base.Reserve(t.Context(), "run", native("one"))
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Capture(t.Context(), CaptureRequest{Source: nil, Gap: Gap{Detail: "x"}}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(base.directory, "run.index.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, extra := range []int{0, 1} {
		s := newStore(t, 1)
		r, err = s.Reserve(t.Context(), "run", native("one"))
		if err != nil {
			t.Fatal(err)
		}
		detail := strings.Repeat("x", (8<<20)-int(info.Size())+1+extra)
		err = r.Capture(t.Context(), CaptureRequest{Source: nil, Gap: Gap{Detail: detail}})
		if (extra == 1) != (err != nil) {
			t.Fatal("index publication bound", extra, err)
		}
	}
}
func TestPublicScanAndResponseBounds(t *testing.T) {
	s := newStore(t, 8<<20)
	raw := bytes.Repeat([]byte{0xff, '\n'}, 2<<20)
	if err := s.Append(t.Context(), "run", raw); err != nil {
		t.Fatal(err)
	}
	body, meta, err := s.Read(t.Context(), "run", ReadRequest{Selector: Selector{}, Filter: Filter{}})
	if err != nil || len(body) != 4<<20 || !meta.Truncated || meta.TotalLines != 0 || meta.MatchedLines != 0 || len(meta.LineNumbers) != 0 {
		t.Fatal(len(body), meta, err)
	}
	offset := int64(1)
	_, meta, err = s.Read(t.Context(), "run", ReadRequest{Selector: Selector{}, Filter: Filter{ByteOffset: &offset}})
	if err != nil || meta.ScannedBytes != 4<<20 {
		t.Fatal(meta, err)
	}
}
func TestPublicRecordAt64KiB(t *testing.T) {
	s := newStore(t, 1<<20)
	exact := strings.Repeat("x", (64<<10)-1) + "\n"
	over := strings.Repeat("y", 64<<10) + "\n"
	if err := s.Append(t.Context(), "run", []byte(exact+over)); err != nil {
		t.Fatal(err)
	}
	body, meta, err := s.Read(t.Context(), "run", ReadRequest{Selector: Selector{}, Filter: Filter{}})
	if err != nil || string(body) != exact || meta.OmittedLongLines != 1 {
		t.Fatal(len(body), meta, err)
	}
}
func TestDefault64MiBCaptureBoundary(t *testing.T) {
	for _, extra := range []int64{0, 1} {
		s := newStore(t, DefaultRunLimitBytes)
		r, err := s.Reserve(t.Context(), "run", native("one"))
		if err != nil {
			t.Fatal(err)
		}
		if err = r.Capture(t.Context(), CaptureRequest{Source: &generatedSource{remaining: (64 << 20) + extra}, Gap: Gap{}}); err != nil {
			t.Fatal(err)
		}
		entries, err := s.Containers(t.Context(), "run")
		if err != nil || entries[0].End != 64<<20 || (entries[0].Reason != "") != (extra == 1) {
			t.Fatal(entries, err)
		}
	}
}

type cancelingReader struct {
	source io.ReaderAt
	cancel context.CancelFunc
	bytes  int
}

func (r *cancelingReader) ReadAt(p []byte, off int64) (int, error) {
	n, err := r.source.ReadAt(p, off)
	r.bytes += n
	r.cancel()
	return n, err
}
func TestCanceledWindowStopsBeforeAnotherBlock(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	source := &cancelingReader{source: bytes.NewReader(bytes.Repeat([]byte("x"), 1<<20)), cancel: cancel}
	_, _, err := readWindow(ctx, source, windows([]span{{0, 1 << 20}}, Filter{}))
	if !errors.Is(err, context.Canceled) || source.bytes != 32<<10 {
		t.Fatal(source.bytes, err)
	}
}
func TestPodMissingUsesClosedReason(t *testing.T) {
	s := newStore(t, 1)
	r, err := s.Reserve(t.Context(), "run", native("one"))
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Capture(t.Context(), CaptureRequest{Source: nil, Gap: Gap{Reason: GapReason("caller status"), Detail: "native detail"}}); err == nil {
		t.Fatal("non-catalogue reason accepted")
	}
	raw, err := json.Marshal(Meta{Tail: true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "discovery_bytes") || strings.Contains(string(raw), "total_scanned_bytes") {
		t.Fatal("redundant fields returned")
	}
}

func TestPublicTailWindowFacts(t *testing.T) {
	s := newStore(t, 1<<20)
	raw := strings.Repeat("line\n", 1000)
	if err := s.Append(t.Context(), "run", []byte(raw)); err != nil {
		t.Fatal(err)
	}
	body, meta, err := s.Read(t.Context(), "run", ReadRequest{Selector: Selector{}, Filter: Filter{Tail: true, Limit: DefaultWindowLines}})
	if err != nil || string(body) != strings.Repeat("line\n", 200) || !meta.Tail || meta.ReturnedLines != 200 || !meta.ScanComplete {
		t.Fatal(meta, err)
	}
}
func TestPublicIndexReadAllocationIsBounded(t *testing.T) {
	s := newStore(t, 1)
	file, err := os.Create(filepath.Join(s.directory, "run.index.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = file.Truncate(64 << 20); err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, _, err = s.Read(t.Context(), "run", ReadRequest{Selector: Selector{}, Filter: Filter{}})
	runtime.ReadMemStats(&after)
	if err == nil || after.TotalAlloc-before.TotalAlloc > 64<<20 {
		t.Fatal("index read exceeded bounded allocation", after.TotalAlloc-before.TotalAlloc, err)
	}
}
