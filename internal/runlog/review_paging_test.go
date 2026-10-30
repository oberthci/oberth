package runlog

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestBoundedTailPagesPastThreeMiBRecord(t *testing.T) {
	for _, active := range []bool{false, true} {
		t.Run(fmt.Sprintf("active-%t", active), func(t *testing.T) {
			first, last := "[t/u] first\n", "[t/u] tail line\n"
			giant := "[t/u] " + strings.Repeat("x", 3<<20) + "\n"
			store := seedRun(t, "giant-review", strings.Split(strings.TrimSuffix(first+giant+last, "\n"), "\n"))
			var cursor *int64
			var pages []string
			omitted := 0
			finished := false
			for range 8 {
				filter := Filter{Tail: true, Limit: 1, ByteOffset: cursor}
				var body []byte
				var meta Meta
				var err error
				if active {
					body, meta, err = store.ReadActiveFiltered("giant-review", "t", "u", filter)
				} else {
					body, meta, err = store.ReadFiltered("giant-review", "t", "u", filter)
				}
				if err != nil {
					t.Fatal(err)
				}
				if meta.ScannedBytes > MaxScanBytes || meta.DiscoveryBytes > MaxScanBytes {
					t.Fatalf("unbounded page: %+v", meta)
				}
				omitted += meta.OmittedLongLines
				if len(body) > 0 {
					pages = append(pages, string(body))
				}
				if !meta.HasMore {
					finished = true
					break
				}
				before := int64(len(first + giant + last))
				if cursor != nil {
					before = *cursor
				}
				if meta.NextByteOffset >= before || meta.NextByteOffset < 0 {
					t.Fatalf("tail stuck: before=%d meta=%+v", before, meta)
				}
				cursor = &meta.NextByteOffset
			}
			if !finished || omitted == 0 || len(pages) != 2 || pages[0] != last || pages[1] != first {
				t.Fatalf("lost records or hidden giant: finished=%t omitted=%d pages=%q", finished, omitted, pages)
			}
		})
	}
}

func TestActiveBoundedWindowNeverReturnsCutOffRecord(t *testing.T) {
	window := int(MaxScanBytes - initialTailBytes - 1)
	giant := "[t/u] " + strings.Repeat("x", window-32-len("[t/u] ")) + "\n"
	final := "[t/u] final 界 " + strings.Repeat("z", 100) + "\n"
	store := seedRun(t, "cut-off-review", []string{strings.TrimSuffix(giant, "\n"), strings.TrimSuffix(final, "\n")})
	cursor := int64(1) // Discovery discards almost its whole window before this newline.
	body, meta, err := store.readActiveWindow("cut-off-review", "t", "u", Filter{ByteOffset: &cursor}, MaxScanBytes)
	if err != nil || len(body) != 0 || !meta.HasMore || meta.NextByteOffset != int64(len(giant)) {
		t.Fatalf("offered a partial record: %q %+v %v", body, meta, err)
	}
	body, meta, err = store.readActiveWindow("cut-off-review", "t", "u", Filter{ByteOffset: &meta.NextByteOffset}, MaxScanBytes)
	if err != nil || string(body) != final || !utf8.Valid(body) || meta.HasMore {
		t.Fatalf("continuation lost complete record: %q %+v %v", body, meta, err)
	}
}

func TestActiveBoundedHeadPagesPastGiantRecord(t *testing.T) {
	first, last := "[t/u] first\n", "[t/u] last\n"
	giant := "[t/u] " + strings.Repeat("x", int(MaxScanBytes)*2) + "\n"
	store := seedRun(t, "head-giant-review", []string{strings.TrimSuffix(first, "\n"), strings.TrimSuffix(giant, "\n"), strings.TrimSuffix(last, "\n")})
	var cursor *int64
	var output strings.Builder
	for page := 0; page < 8; page++ {
		body, meta, err := store.ReadActiveFiltered("head-giant-review", "t", "u", Filter{ByteOffset: cursor})
		if err != nil {
			t.Fatal(err)
		}
		output.Write(body)
		if !meta.HasMore {
			if output.String() != first+last {
				t.Fatalf("lost head records: %q", output.String())
			}
			return
		}
		before := int64(0)
		if cursor != nil {
			before = *cursor
		}
		if meta.NextByteOffset <= before {
			t.Fatalf("head stuck: %+v", meta)
		}
		cursor = &meta.NextByteOffset
	}
	t.Fatal("head did not finish")
}

func TestActiveBoundedDiscoveryDoesNotCountAnotherStepsGiantRecord(t *testing.T) {
	wanted := "[wanted/step] hello"
	other := "[other/step] " + strings.Repeat("x", int(MaxScanBytes)*2)
	store := seedRun(t, "other-giant-review", []string{wanted, other})
	body, meta, err := store.ReadActiveFiltered("other-giant-review", "wanted", "step", Filter{})
	if err != nil || string(body) != wanted+"\n" || meta.OmittedLongLines != 0 || meta.TotalLines != 1 {
		t.Fatalf("another step inflated counts: %q %+v %v", body, meta, err)
	}
}
