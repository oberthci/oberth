package runlog

import (
	"bytes"
	"slices"
	"testing"
)

func TestAbsentMatchesRevisitWholeRecordsAcrossWindowBoundaries(t *testing.T) {
	large := bytes.Repeat([]byte("x\n"), (int(MaxScanBytes)-10)/2)
	small := bytes.Repeat([]byte("x\n"), 3000)
	wanted := []byte("match 界 boundary\n")
	for _, tail := range []bool{false, true} {
		before, after := large, small
		if tail {
			before, after = small, large
		}
		raw := bytes.Join([][]byte{before, wanted, after}, nil)
		for _, pattern := range []string{"", "^match"} {
			expected := raw
			if pattern != "" {
				expected = wanted
			}
			actual := boundaryPages(t, raw, Filter{Tail: tail, Pattern: pattern})
			if !bytes.Equal(actual, expected) {
				t.Fatal("bounded edge record was lost or duplicated", tail, pattern)
			}
		}
	}
}
func boundaryPages(t *testing.T, raw []byte, filter Filter) []byte {
	t.Helper()
	var pages [][]byte
	for range 10 {
		reader := &countedReader{ReaderAt: bytes.NewReader(raw)}
		body, meta, err := scan(t.Context(), reader, []span{{0, int64(len(raw))}}, filter)
		if err != nil || reader.bytes > MaxScanBytes || reader.bytes != meta.ScannedBytes {
			t.Fatal(meta, reader.bytes, err)
		}
		pages = append(pages, body)
		if !meta.HasMore {
			if filter.Tail {
				slices.Reverse(pages)
			}
			return bytes.Join(pages, nil)
		}
		next := meta.NextByteOffset
		if filter.ByteOffset != nil && (filter.Tail && next >= *filter.ByteOffset || !filter.Tail && next <= *filter.ByteOffset) {
			t.Fatal("cursor stalled", meta)
		}
		filter.ByteOffset = &next
	}
	t.Fatal("bounded paging did not finish")
	return nil
}
