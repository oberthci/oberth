package runlog

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestPublicBudgetsAndWireMetadata(t *testing.T) {
	raw, err := json.Marshal(Meta{LineNumbers: []int{1}, LineOffsets: []int64{0}})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range strings.Fields("scan_limit_bytes total_lines matched_lines returned_lines truncated bytes line_numbers line_offsets scan_complete bytes_exact scanned_bytes withheld_bytes withheld_lines omitted_long_lines has_more next_byte_offset") {
		if _, ok := fields[key]; !ok {
			t.Fatal("missing wire metadata", key)
		}
	}
}

func TestOrdinaryAppendCapHasAReadableReasonWithoutAddingText(t *testing.T) {
	s := newStore(t, 3)
	if err := s.Append(t.Context(), "run", []byte("abc ignored")); err != nil {
		t.Fatal(err)
	}
	body, meta, err := s.Read(t.Context(), "run", ReadRequest{Selector: Selector{}, Filter: Filter{}})
	if err != nil || string(body) != "abc" || !meta.Truncated || !strings.Contains(meta.Reason, "byte limit") {
		t.Fatal(string(body), meta, err)
	}
}

func TestWindowOverflowAndLineNumbersAcrossOmittedRecords(t *testing.T) {
	raw := []byte(strings.Repeat("x", (64<<10)+1) + "\nkept\nlast\n")
	body, meta, err := scan(t.Context(), strings.NewReader(string(raw)), []span{{0, int64(len(raw))}}, Filter{Offset: 1, Limit: int(^uint(0) >> 1)})
	if err != nil || string(body) != "last\n" || len(meta.LineNumbers) != 1 || meta.LineNumbers[0] != 3 || meta.OmittedLongLines != 1 {
		t.Fatal(string(body), meta, err)
	}
}

func TestLargestPositiveLimitDoesNotOverflowCaptureProbe(t *testing.T) {
	s := newStore(t, math.MaxInt64)
	capture(t, s, native("one"), "output\n")
	body, _, err := s.Read(t.Context(), "run", ReadRequest{Selector: Selector{}, Filter: Filter{}})
	if err != nil || string(body) != "output\n" {
		t.Fatal(string(body), err)
	}
}
