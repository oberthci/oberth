package runlog

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func TestPerCallScanAccountingReportsHardAggregateBounds(t *testing.T) {
	raw := strings.Repeat("[t/u] noise\n", 1000000)
	store := seedRun(t, "accounting", strings.Split(strings.TrimSuffix(raw, "\n"), "\n"))
	for _, active := range []bool{false, true} {
		for _, tail := range []bool{false, true} {
			t.Run(fmt.Sprintf("active=%t/tail=%t", active, tail), func(t *testing.T) {
				filter := Filter{Pattern: "absent", Tail: tail, Limit: 20}
				var meta Meta
				var err error
				if active {
					_, meta, err = store.ReadActiveFiltered("accounting", "t", "u", filter)
				} else {
					_, meta, err = store.ReadFiltered("accounting", "t", "u", filter)
				}
				if err != nil {
					t.Fatal(err)
				}
				want := MaxScanBytes
				if active {
					want = MaxCallScanBytes
				}
				if meta.ScanLimitBytes != want || meta.TotalScannedBytes != meta.ScannedBytes+meta.DiscoveryBytes || meta.TotalScannedBytes > want || meta.ScanComplete || !meta.HasMore {
					t.Fatalf("missing/incorrect cap report: %+v", meta)
				}
			})
		}
	}
}

func TestPerCallTerminalAccountingIncludesActualReadAheadAndTailRereads(t *testing.T) {
	raw := []byte(strings.Repeat("[t/u] noise\n", 1000000))
	reader := &countedLogReader{ReaderAt: bytes.NewReader(raw)}
	_, meta, err := scanBoundedRanges(reader, []Range{{End: int64(len(raw))}}, int64(len(raw)), Filter{Pattern: "absent", Tail: true, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if meta.TotalScannedBytes != reader.bytes || meta.TotalScannedBytes > meta.ScanLimitBytes || meta.ScanLimitBytes != MaxScanBytes {
		t.Fatalf("accounting hid physical log reads: actual=%d meta=%+v", reader.bytes, meta)
	}
}
