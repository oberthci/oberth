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
	activeStore := runningLog(t, "accounting", raw)
	for _, active := range []bool{false, true} {
		for _, tail := range []bool{false, true} {
			t.Run(fmt.Sprintf("active=%t/tail=%t", active, tail), func(t *testing.T) {
				filter := Filter{Pattern: "absent", Tail: tail, Limit: 20}
				var meta Meta
				var err error
				if active {
					_, meta, err = activeStore.ReadActiveFiltered("accounting", "t", "u", filter)
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

// A cold head uses indexed ranges; a cold tail also discovers a raw window.
// Check Meta directly because the service independently recomputes the sum.
func TestPerCallRunningIndexAccountingIncludesFallbackDiscovery(t *testing.T) {
	raw := strings.Repeat("[t/u] noise\n", 1000000)
	for _, tail := range []bool{false, true} {
		t.Run(fmt.Sprintf("tail=%t", tail), func(t *testing.T) {
			store := runningLog(t, "accounting", raw)
			_, meta, err := store.ReadActiveFiltered("accounting", "t", "u", Filter{Pattern: "absent", Tail: tail, Limit: 20})
			if err != nil {
				t.Fatal(err)
			}
			if meta.ScanLimitBytes != MaxCallScanBytes || meta.TotalScannedBytes != meta.ScannedBytes+meta.DiscoveryBytes || meta.TotalScannedBytes > meta.ScanLimitBytes {
				t.Fatalf("wrong active aggregate accounting: %+v", meta)
			}
			if (!tail && meta.DiscoveryBytes != runningIndexAdvance) || (tail && meta.DiscoveryBytes <= runningIndexAdvance) || meta.ScannedBytes == 0 {
				t.Fatalf("did not exercise expected indexed/fallback path: %+v", meta)
			}
		})
	}
	store := runningLog(t, "warm", "[t/u] first\n")
	for call := range 3 {
		_, meta, err := store.ReadActiveFiltered("warm", "t", "u", Filter{})
		if err != nil {
			t.Fatal(err)
		}
		if meta.ScanLimitBytes != MaxCallScanBytes || meta.TotalScannedBytes != meta.ScannedBytes+meta.DiscoveryBytes || meta.TotalScannedBytes > meta.ScanLimitBytes {
			t.Fatalf("wrong warm/append accounting: %+v", meta)
		}
		if call == 1 {
			if meta.DiscoveryBytes != 0 {
				t.Fatalf("unchanged log rediscovered: %+v", meta)
			}
			appendRunning(t, store, "warm", "[t/u] appended\n")
		}
		if call == 2 && meta.DiscoveryBytes != int64(len("[t/u] appended\n"))+1 {
			t.Fatalf("append boundary probe not counted: %+v", meta)
		}
	}
}
