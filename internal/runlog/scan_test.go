package runlog

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestPagingBothDirectionsAgainstIndependentLineOracle(t *testing.T) {
	random := rand.New(rand.NewPCG(1, 2))
	for _, tail := range []bool{false, true} {
		var expected []byte
		for i := range 500 {
			expected = fmt.Appendf(expected, "%d 界 %s\n", i, strings.Repeat("x", random.IntN(80)))
		}
		pages := cursorPages(t, expected, tail)
		if tail {
			for i, j := 0, len(pages)-1; i < j; i, j = i+1, j-1 {
				pages[i], pages[j] = pages[j], pages[i]
			}
		}
		if !bytes.Equal(bytes.Join(pages, nil), expected) {
			t.Fatal("paging lost or duplicated records", tail)
		}
	}
}
func TestDisjointSpansAndAnchoredPatternsArePlain(t *testing.T) {
	raw := []byte("ordinary\n[first] alpha\nHIDDEN\n[second] beta\n")
	spans := []span{{9, 23}, {30, int64(len(raw))}}
	body, meta, err := scan(t.Context(), bytes.NewReader(raw), spans, Filter{Pattern: `^\[`, Context: 0})
	if err != nil || string(body) != "[first] alpha\n[second] beta\n" {
		t.Fatal(string(body), meta, err)
	}
	if meta.MatchedLines != 2 {
		t.Fatal(meta)
	}
	body, _, err = scan(t.Context(), bytes.NewReader(raw), spans, Filter{Pattern: `^alpha`})
	if err != nil || len(body) != 0 {
		t.Fatal("prefix stripping changed the pattern", string(body), err)
	}
}

type countedReader struct {
	io.ReaderAt
	bytes int64
}

func (r *countedReader) ReadAt(p []byte, at int64) (int, error) {
	n, err := r.ReaderAt.ReadAt(p, at)
	r.bytes += int64(n)
	return n, err
}
func TestFourMiBBudgetIncludesEveryReadAndGiantRecordProgress(t *testing.T) {
	raw := []byte(strings.Repeat("界", 3<<20) + "\nlast\n")
	for _, tail := range []bool{false, true} {
		assertBoundedGiant(t, raw, tail)
	}
}
func assertBoundedGiant(t *testing.T, raw []byte, tail bool) {
	t.Helper()
	reader := &countedReader{ReaderAt: bytes.NewReader(raw)}
	body, meta, err := scan(t.Context(), reader, []span{{0, int64(len(raw))}}, Filter{Pattern: "missing", Tail: tail})
	assertScanBudget(t, meta, reader.bytes, err)
	if len(body) > int(MaxScanBytes) {
		t.Fatal("output bound exceeded")
	}
	if !utf8.Valid(body) || meta.OmittedLongLines == 0 {
		t.Fatal("giant record was not explicit", meta)
	}
	if !tail && meta.NextByteOffset <= 0 || tail && meta.NextByteOffset >= int64(len(raw)) {
		t.Fatal("giant record cursor stalled", meta)
	}
}
func TestUTF8ScanWindowsAndContextOffsetTail(t *testing.T) {
	raw := []byte("one\nhit\nthree\nfour\nhit\nsix\n")
	body, meta, err := scan(t.Context(), bytes.NewReader(raw), []span{{0, int64(len(raw))}}, Filter{Pattern: "hit", Context: 1, Offset: 1, Limit: 3})
	if err != nil || string(body) != "hit\nthree\nfour\n" || meta.MatchedLines != 2 {
		t.Fatal(string(body), meta, err)
	}
	raw = []byte(strings.Repeat("界\n", 2<<20))
	var joined []byte
	var cursor *int64
	for range 5 {
		body, meta, err = scan(t.Context(), bytes.NewReader(raw), []span{{0, int64(len(raw))}}, Filter{ByteOffset: cursor})
		if err != nil || !utf8.Valid(body) {
			t.Fatal(err)
		}
		joined = append(joined, body...)
		if !meta.HasMore {
			break
		}
		value := meta.NextByteOffset
		cursor = &value
	}
	if !bytes.Equal(joined, raw) {
		t.Fatal("bounded UTF-8 windows lost bytes")
	}
}
func TestFilterAndSpanRefusalsAreSpecific(t *testing.T) {
	negative := int64(-1)
	for _, filter := range []Filter{{Pattern: "["}, {Pattern: strings.Repeat("a", 513)}, {Context: -1}, {Context: 51}, {Offset: -1}, {Limit: -1}, {ByteOffset: &negative}} {
		if _, _, err := scan(t.Context(), bytes.NewReader(nil), nil, filter); err == nil {
			t.Fatal("invalid filter accepted", filter)
		}
	}
	for _, spans := range [][]span{{{-1, 2}}, {{2, 1}}, {{0, 4}}, {{0, 2}, {1, 3}}} {
		if err := validSpans(spans, 3); err == nil {
			t.Fatal("invalid range", spans)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := scan(ctx, bytes.NewReader([]byte("x")), []span{{0, 1}}, Filter{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, _, err := scan(t.Context(), errorReader{}, []span{{0, 1}}, Filter{}); err == nil {
		t.Fatal("read failure hidden")
	}
}

type errorReader struct{}

func (errorReader) ReadAt([]byte, int64) (int, error) { return 0, io.ErrUnexpectedEOF }
func cursorPages(t *testing.T, expected []byte, tail bool) [][]byte {
	t.Helper()
	var pages [][]byte
	var cursor *int64
	for range 1000 {
		body, meta, err := scan(t.Context(), bytes.NewReader(expected), []span{{0, int64(len(expected))}}, Filter{Tail: tail, Limit: 7, ByteOffset: cursor})
		if err != nil || !utf8.Valid(body) {
			t.Fatal(err)
		}
		pages = append(pages, body)
		if !meta.HasMore {
			break
		}
		if cursor != nil && (tail && meta.NextByteOffset >= *cursor || !tail && meta.NextByteOffset <= *cursor) {
			t.Fatal("cursor stalled", meta)
		}
		value := meta.NextByteOffset
		cursor = &value
	}
	return pages
}

func assertScanBudget(t *testing.T, meta Meta, actual int64, err error) {
	t.Helper()
	if err != nil || meta.ScannedBytes != actual || actual > MaxScanBytes || !meta.HasMore || meta.ScanComplete {
		t.Fatal(meta, actual, err)
	}
}

func TestDisjointSpanPagingAgainstIndependentByteOracle(t *testing.T) {
	var raw, oracle []byte
	var spans []span
	random := rand.New(rand.NewPCG(33, 44))
	for i := range 100 {
		raw = append(raw, []byte("excluded\n")...)
		start := len(raw)
		body := fmt.Sprintf("[%d] 界 %s\n", i, strings.Repeat("x", random.IntN(50)))
		raw = append(raw, body...)
		spans = append(spans, span{int64(start), int64(len(raw))})
		oracle = append(oracle, body...)
	}
	for _, tail := range []bool{false, true} {
		pages := spanPages(t, raw, spans, tail)
		if tail {
			for i, j := 0, len(pages)-1; i < j; i, j = i+1, j-1 {
				pages[i], pages[j] = pages[j], pages[i]
			}
		}
		if !bytes.Equal(bytes.Join(pages, nil), oracle) {
			t.Fatal("disjoint paging changed selected bytes", tail)
		}
	}
}
func spanPages(t *testing.T, raw []byte, spans []span, tail bool) [][]byte {
	t.Helper()
	var pages [][]byte
	var cursor *int64
	for range 100 {
		body, meta, err := scan(t.Context(), bytes.NewReader(raw), spans, Filter{Tail: tail, Limit: 3, ByteOffset: cursor})
		if err != nil {
			t.Fatal(err)
		}
		pages = append(pages, body)
		if !meta.HasMore {
			return pages
		}
		next := meta.NextByteOffset
		cursor = &next
	}
	t.Fatal("span paging did not terminate")
	return nil
}

func TestTailReplacementBudgetKeepsNewestRecords(t *testing.T) {
	raw := bytes.Repeat([]byte{0xff, '\n'}, int(MaxScanBytes)/2)
	var pages [][]byte
	var cursor *int64
	for range 10 {
		body, meta, err := scan(t.Context(), bytes.NewReader(raw), []span{{0, int64(len(raw))}}, Filter{Tail: true, ByteOffset: cursor})
		if err != nil || !utf8.Valid(body) || len(body) > int(MaxScanBytes) {
			t.Fatal(meta, err)
		}
		pages = append(pages, body)
		if !meta.HasMore {
			break
		}
		next := meta.NextByteOffset
		cursor = &next
	}
	var total int
	for _, body := range pages {
		total += bytes.Count(body, []byte("\n"))
	}
	if total != len(raw)/2 {
		t.Fatal("UTF-8 normalization lost tail records", total)
	}
}
