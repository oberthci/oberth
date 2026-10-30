package runlog

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/runprogress"
)

func runningLog(t *testing.T, id, body string) *Store {
	t.Helper()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f, err := store.Create(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(body); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return store
}
func appendRunning(t *testing.T, store *Store, id, body string) {
	t.Helper()
	path, _ := store.logPath(id)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(body); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRunningIndexFindsLateStepWithoutRepeatingDiscovery(t *testing.T) {
	noise := strings.Repeat("[other/step] noise\n", 360000)
	wanted := "[wanted/step] actual failure\n"
	store := runningLog(t, "late", noise+wanted)
	var discovered int64
	found := false
	for range 8 {
		body, meta, err := store.ReadActiveFiltered("late", "wanted", "step", Filter{Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		if meta.DiscoveryBytes > MaxScanBytes || meta.ScannedBytes > MaxScanBytes {
			t.Fatalf("unbounded read: %+v", meta)
		}
		discovered += meta.DiscoveryBytes
		if len(body) > 0 {
			if string(body) != wanted || !meta.BytesExact || meta.HasMore {
				t.Fatalf("wrong late step: %q %+v", body, meta)
			}
			found = true
			break
		}
		if !meta.HasMore || meta.BytesExact || meta.NextByteOffset == 0 {
			t.Fatalf("cold incomplete read was hidden: %+v", meta)
		}
	}
	if !found || discovered != int64(len(noise+wanted)) {
		t.Fatalf("index did not advance once over existing bytes: found=%t read=%d", found, discovered)
	}
	for range 3 {
		body, meta, err := store.ReadActiveFiltered("late", "wanted", "step", Filter{Tail: true, Limit: 1})
		if err != nil || string(body) != wanted || meta.DiscoveryBytes != 0 {
			t.Fatalf("unchanged active log was reindexed: %q %+v %v", body, meta, err)
		}
	}
	appendRunning(t, store, "late", "[wanted/step] new bytes\n")
	body, meta, err := store.ReadActiveFiltered("late", "wanted", "step", Filter{Tail: true, Limit: 1})
	if err != nil || string(body) != "[wanted/step] new bytes\n" || meta.DiscoveryBytes != int64(len("[wanted/step] new bytes\n"))+1 {
		t.Fatalf("append was not incremental: %q %+v %v", body, meta, err)
	}
}

func TestRunningIndexCarriesPartialRecordsAndDeliveryAttribution(t *testing.T) {
	raw := "[a/s] first\ncontinuation\n" + runprogress.LogDeliveryPrefix + "gap\nnot a step\n[b/u] 界 last\n"
	entry := &runningIndex{}
	for size := int64(1); size <= int64(len(raw)); size++ {
		read, err := entry.advance(t.Context(), bytes.NewReader([]byte(raw)), size)
		if err != nil || read != 1 {
			t.Fatalf("increment at%d: %d %v", size, read, err)
		}
	}
	index, err := indexReader("partial", strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	actual, _, _, _ := entry.snapshot("a", "", Filter{})
	other, _, _, _ := entry.snapshot("b", "", Filter{})
	actual = append(actual, other...)
	if !reflect.DeepEqual(actual, index.Ranges) {
		t.Fatalf("incremental attribution differs: %+v / %+v", actual, index.Ranges)
	}
	store := runningLog(t, "partial", "[a/s] first\n[b/u] 界")
	_, _, err = store.ReadActiveFiltered("partial", "a", "s", Filter{})
	if err != nil {
		t.Fatal(err)
	}
	appendRunning(t, store, "partial", " last\n")
	body, meta, err := store.ReadActiveFiltered("partial", "b", "u", Filter{})
	if err != nil || string(body) != "[b/u] 界 last\n" || meta.DiscoveryBytes != int64(len(" last\n"))+1 {
		t.Fatalf("partial UTF8 record was reread/lost: %q %+v %v", body, meta, err)
	}
}

func TestRunningIndexColdTailAndGiantRecordsKeepCursorProgress(t *testing.T) {
	first, last := "[t/u] first\n", "[t/u] last\n"
	raw := first + "[t/u] " + strings.Repeat("x", int(MaxScanBytes)*2) + "\n" + last
	store := runningLog(t, "giant", raw)
	var cursor *int64
	var output []string
	for range 16 {
		body, meta, err := store.ReadActiveFiltered("giant", "t", "u", Filter{Tail: true, Limit: 1, ByteOffset: cursor})
		if err != nil {
			t.Fatal(err)
		}
		if meta.DiscoveryBytes > MaxScanBytes || meta.ScannedBytes > MaxScanBytes {
			t.Fatalf("budget exceeded: %+v", meta)
		}
		if len(body) > 0 {
			output = append(output, string(body))
		}
		if !meta.HasMore {
			if !reflect.DeepEqual(output, []string{last, first}) {
				t.Fatalf("lost tail data: %q", output)
			}
			return
		}
		before := int64(len(raw))
		if cursor != nil {
			before = *cursor
		}
		if meta.NextByteOffset >= before {
			t.Fatalf("tail stuck: %+v", meta)
		}
		cursor = &meta.NextByteOffset
	}
	t.Fatal("tail never finished")
}

func TestRunningIndexResetsAfterReplacementAndTruncation(t *testing.T) {
	store := runningLog(t, "replace", "[a/s] old\n")
	_, _, err := store.ReadActiveFiltered("replace", "a", "s", Filter{})
	if err != nil {
		t.Fatal(err)
	}
	path, _ := store.logPath("replace")
	replacement := path + ".new"
	if err := os.WriteFile(replacement, []byte("[b/u] new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	body, meta, err := store.ReadActiveFiltered("replace", "b", "u", Filter{})
	if err != nil || string(body) != "[b/u] new\n" || meta.DiscoveryBytes != int64(len("[b/u] new\n")) {
		t.Fatalf("replacement kept stale index: %q %+v %v", body, meta, err)
	}
	if err := os.WriteFile(path, []byte("[c/v] x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	body, _, err = store.ReadActiveFiltered("replace", "c", "v", Filter{})
	if err != nil || string(body) != "[c/v] x\n" {
		t.Fatalf("truncation kept stale index: %q %v", body, err)
	}
}

func TestRunningIndexMemoryBoundsAndLifecycle(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxRunningIndexes+2; i++ {
		id := fmt.Sprintf("r-%03d", i)
		f, err := store.Create(id)
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.WriteString("[a/s] line\n")
		if err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
		if _, _, err := store.ReadActiveFiltered(id, "a", "s", Filter{}); err != nil {
			t.Fatal(err)
		}
	}
	if len(store.running) != maxRunningIndexes || store.running["r-000"] != nil {
		t.Fatal("cache failed to evict idle LRU entries")
	}
	if _, err := store.BuildIndex("r-065"); err != nil {
		t.Fatal(err)
	}
	if store.running["r-065"] != nil {
		t.Fatal("finish retained running index")
	}
	if _, _, err := store.ReadActiveFiltered("r-065", "a", "s", Filter{}); err != nil {
		t.Fatal(err)
	}
	if store.running["r-065"] != nil {
		t.Fatal("late active read resurrected finished index")
	}
	if _, err := store.Purge("r-064"); err != nil {
		t.Fatal(err)
	}
	if store.running["r-064"] != nil {
		t.Fatal("purge retained index")
	}

}

func TestRunningIndexConcurrentAppendReadAndFinish(t *testing.T) {
	store := runningLog(t, "race", "[a/s] start\n")
	path, _ := store.logPath("race")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	wg.Go(func() {
		defer file.Close()
		for range 100 {
			if _, err := file.WriteString("[a/s] append\n"); err != nil {
				errs <- err
				return
			}
		}
	})
	for range 3 {
		wg.Go(func() {
			for range 30 {
				_, meta, err := store.ReadActiveFiltered("race", "a", "s", Filter{Tail: true, Limit: 2})
				if err != nil {
					errs <- err
					return
				}
				if meta.DiscoveryBytes > MaxScanBytes {
					errs <- fmt.Errorf("discovery budget exceeded")
					return
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if _, err := store.BuildIndex("race"); err != nil {
		t.Fatal(err)
	}
	if len(store.running) != 0 {
		t.Fatal("finished cache remains")
	}
}

func TestRunningIndexWaitHonorsContext(t *testing.T) {
	store := runningLog(t, "blocked", "[a/s] data\n")
	entry := store.pinRunningIndex("blocked")
	entry.gate <- struct{}{}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	_, _, err := store.ReadActiveFiltered("blocked", "a", "s", Filter{}.WithContext(ctx))
	<-entry.gate
	store.unpinRunningIndex(entry)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("same-run index wait ignored deadline: %v", err)
	}
}

func TestRunningIndexMarkerStorageIsBounded(t *testing.T) {
	label := strings.Repeat("b", 60000)
	raw := "[" + label + "/s0] x\n[" + label + "/s1] x\n[" + label + "/s2] x\n[wanted/s] final\n"
	store := runningLog(t, "labels", raw)
	body, meta, err := store.ReadActiveFiltered("labels", "wanted", "s", Filter{Tail: true})
	entry := store.running["labels"]
	if err != nil || string(body) != "[wanted/s] final\n" || entry.floor == 0 || entry.labelBytes > maxRunningLabelBytes || len(entry.labelIDs) != 3 || meta.DiscoveryBytes > MaxScanBytes {
		t.Fatalf("label bound/fallback failed: %+v %v", meta, err)
	}
}

func TestRunningIndexLateReadersCannotResurrectFinishedEntry(t *testing.T) {
	store := runningLog(t, "finish", strings.Repeat("[a/s] data\n", 1000))
	if _, _, err := store.ReadActiveFiltered("finish", "a", "s", Filter{Limit: 1}); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			<-start
			for range 20 {
				if _, _, err := store.ReadActiveFiltered("finish", "a", "s", Filter{Tail: true, Limit: 1}); err != nil {
					errs <- err
					return
				}
			}
		})
	}
	close(start)
	if _, err := store.BuildIndex("finish"); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if len(store.running) != 0 {
		t.Fatal("late reader resurrected finished cache")
	}
}

func TestRunningIndexAdvanceReadsOnlyNewBytesWithinBudget(t *testing.T) {
	raw := "[a/s] " + strings.Repeat("x", int(runningIndexAdvance)*2) + "\n[a/s] last\n"
	reader := &countingReaderAt{reader: strings.NewReader(raw)}
	entry := &runningIndex{}
	for entry.position < int64(len(raw)) {
		before := entry.position
		reader.read = 0
		discovered, err := entry.advance(t.Context(), reader, int64(len(raw)))
		if err != nil || discovered != min(runningIndexAdvance, int64(len(raw))-before) || reader.read != discovered || entry.position != before+discovered || cap(entry.prefix) > maxMarkerBytes {
			t.Fatalf("advance reread or exceeded bounds: before=%d position=%d discovered=%d actual=%d err=%v", before, entry.position, discovered, reader.read, err)
		}
	}
	reader.read = 0
	discovered, err := entry.advance(t.Context(), reader, int64(len(raw)))
	if err != nil || discovered != 0 || reader.read != 0 {
		t.Fatalf("unchanged log read bytes: %d %d %v", discovered, reader.read, err)
	}
}

type countingReaderAt struct {
	reader *strings.Reader
	read   int64
}

func (reader *countingReaderAt) ReadAt(p []byte, offset int64) (int, error) {
	n, err := reader.reader.ReadAt(p, offset)
	reader.read += int64(n)
	return n, err
}

func TestRunningIndexPinnedCapacityFallsBackWithoutGrowingCache(t *testing.T) {
	store := runningLog(t, "fallback", "[a/s] final\n")
	for i := range maxRunningIndexes {
		entry := store.pinRunningIndex(fmt.Sprintf("pinned-%d", i))
		if entry == nil {
			t.Fatal("unexpected early cache exhaustion")
		}
		defer store.unpinRunningIndex(entry)
	}
	body, meta, err := store.ReadActiveFiltered("fallback", "a", "s", Filter{})
	if err != nil || string(body) != "[a/s] final\n" || len(store.running) != maxRunningIndexes || store.running["fallback"] != nil || meta.DiscoveryBytes > MaxScanBytes {
		t.Fatalf("pinned capacity violated cache/read bounds: %+v %v", meta, err)
	}
	indexPath, _ := store.indexPath("fallback")
	if _, err := os.Stat(indexPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("active read published a terminal index: %v", err)
	}
}

func TestRunningIndexInterleavingKeepsAdvancingPastOldCap(t *testing.T) {
	var raw strings.Builder
	for i := range 8000 {
		fmt.Fprintf(&raw, "[parallel/s%d] line\n", i%2)
	}
	raw.WriteString(strings.Repeat("[noise/step] noise\n", 360000))
	wanted := "[late/step] found\n"
	raw.WriteString(wanted)
	store := runningLog(t, "interleaved", raw.String())
	var discovery int64
	found := false
	for range 12 {
		body, meta, err := store.ReadActiveFiltered("interleaved", "late", "step", Filter{Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		discovery += meta.DiscoveryBytes
		if string(body) == wanted {
			found = true
			break
		}
		if !meta.HasMore {
			t.Fatalf("lost late-step progress: %+v", meta)
		}
	}
	entry := store.running["interleaved"]
	if !found || discovery != int64(raw.Len()) || entry.count < 8000 || len(entry.labelIDs) != 4 || entry.labelBytes != len("parallels0parallels1noisesteplatestep") {
		t.Fatalf("interleaving lost index or duplicated labels: found=%v read=%d ranges=%d labels=%d labelBytes=%d", found, discovery, entry.count, len(entry.labelIDs), entry.labelBytes)
	}
	for range 3 {
		body, meta, err := store.ReadActiveFiltered("interleaved", "late", "step", Filter{Tail: true, Limit: 1})
		if err != nil || string(body) != wanted || meta.DiscoveryBytes != 0 {
			t.Fatalf("interleaving became sticky fallback: %q %+v %v", body, meta, err)
		}
	}
}

func TestRunningIndexRollingSuffixRetainsNewOutputAndOldPaging(t *testing.T) {
	var raw strings.Builder
	raw.WriteString("[a/s0] first\n")
	for i := range maxRunningRanges + 200 {
		fmt.Fprintf(&raw, "[a/s%d] old\n", i%2)
	}
	store := runningLog(t, "rolling", raw.String())
	body, meta, err := store.ReadActiveFiltered("rolling", "a", "s1", Filter{Tail: true, Limit: 1})
	entry := store.running["rolling"]
	if err != nil || string(body) != "[a/s1] old\n" || entry.floor == 0 || entry.count > maxRunningRanges || meta.BytesExact || !meta.HasMore {
		t.Fatalf("overflow failed to retain newest: %q %+v entry=%+v err=%v", body, meta, entry, err)
	}
	next := "[a/s1] new\n"
	appendRunning(t, store, "rolling", next)
	body, meta, err = store.ReadActiveFiltered("rolling", "a", "s1", Filter{Tail: true, Limit: 1})
	if err != nil || string(body) != next || meta.DiscoveryBytes != int64(len(next))+1 || entry.position != int64(raw.Len()+len(next)) {
		t.Fatalf("overflow stopped advancing: %q %+v %v", body, meta, err)
	}
	cursor := entry.floor
	body, meta, err = store.ReadActiveFiltered("rolling", "a", "s0", Filter{Tail: true, Pattern: "first", ByteOffset: &cursor})
	if err != nil || string(body) != "[a/s0] first\n" || meta.HasMore || meta.DiscoveryBytes > MaxScanBytes {
		t.Fatalf("dropped prefix became inaccessible: %q %+v %v", body, meta, err)
	}
	cursor = entry.position
	finished := false
	for range 12 {
		body, meta, err = store.ReadActiveFiltered("rolling", "absent", "step", Filter{Tail: true, ByteOffset: &cursor})
		if err != nil {
			t.Fatal(err)
		}
		if !meta.HasMore {
			finished = true
			break
		}
		if meta.NextByteOffset >= cursor {
			t.Fatalf("suffix/prefix tail handoff stalled: %q %+v", body, meta)
		}
		cursor = meta.NextByteOffset
	}
	if !finished {
		t.Fatal("suffix/prefix handoff never finished")
	}
}

func TestRunningIndexGlobalRangeBudget(t *testing.T) {
	var raw strings.Builder
	for i := range maxRunningRanges + 2 {
		fmt.Fprintf(&raw, "[a/s%d] x\n", i%2)
	}
	for _, pinned := range []bool{false, true} {
		t.Run(fmt.Sprintf("pinned-%t", pinned), func(t *testing.T) {
			store, err := Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			var entries []*runningIndex
			if pinned {
				for i := range 9 {
					entries = append(entries, store.pinRunningIndex(fmt.Sprintf("r-%d", i)))
				}
			}
			for i := range 9 {
				var entry *runningIndex
				if pinned {
					entry = entries[i]
				} else {
					entry = store.pinRunningIndex(fmt.Sprintf("r-%d", i))
				}
				if entry == nil {
					t.Fatal("could not reserve a bounded initial entry")
				}
				if _, err := entry.advance(t.Context(), strings.NewReader(raw.String()), int64(raw.Len())); err != nil {
					t.Fatal(err)
				}
				if entry.position != int64(raw.Len()) || store.runningSlots > maxRunningRangeSlots {
					t.Fatal("global capacity overflow or stopped index")
				}
				if !pinned {
					store.unpinRunningIndex(entry)
				}
			}
			if pinned {
				if entries[8].floor == 0 {
					t.Fatal("pinned budget pressure failed to roll own suffix")
				}
				for _, entry := range entries {
					store.unpinRunningIndex(entry)
				}
			} else if store.running["r-0"] != nil {
				t.Fatal("range pressure failed to evict idle LRU")
			}
			for id := range store.running {
				store.ForgetActiveIndex(id)
			}
			if store.runningSlots != 0 || store.runningEntries != 0 {
				t.Fatalf("budget leaked after release: slots=%d entries=%d", store.runningSlots, store.runningEntries)
			}
		})
	}
}

func TestRunningIndexDetachedReadersRemainCharged(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	entries := make([]*runningIndex, maxRunningIndexes)
	for i := range entries {
		entries[i] = store.pinRunningIndex(fmt.Sprintf("r-%d", i))
	}
	store.ForgetActiveIndex("r-0")
	if store.runningEntries != maxRunningIndexes || store.pinRunningIndex("replacement") != nil {
		t.Fatal("detached readers bypassed global entry budget")
	}
	store.unpinRunningIndex(entries[0])
	replacement := store.pinRunningIndex("replacement")
	if replacement == nil {
		t.Fatal("last detached reader did not return capacity")
	}
	store.unpinRunningIndex(replacement)
	for _, entry := range entries[1:] {
		store.unpinRunningIndex(entry)
	}
	for id := range store.running {
		store.ForgetActiveIndex(id)
	}
	if store.runningSlots != 0 || store.runningEntries != 0 {
		t.Fatal("detached entry accounting leaked")
	}
}

func TestRunningIndexTruncateRegrowChecksNewlineBoundary(t *testing.T) {
	old := "[a/s] one\n[b/s] two\n"
	store := runningLog(t, "regrow", old)
	if _, _, err := store.ReadActiveFiltered("regrow", "a", "s", Filter{}); err != nil {
		t.Fatal(err)
	}
	replacement := "[b/s] " + strings.Repeat("x", len(old)+20) + "\n[a/s] new-a\n"
	path, _ := store.logPath("regrow")
	if err := os.WriteFile(path, []byte(replacement), 0o600); err != nil {
		t.Fatal(err)
	}
	body, meta, err := store.ReadActiveFiltered("regrow", "a", "s", Filter{})
	if err != nil || string(body) != "[a/s] new-a\n" || meta.DiscoveryBytes != int64(len(replacement))+1 {
		t.Fatalf("regrow spliced another step: %q %+v %v", body, meta, err)
	}
}

func TestRunningIndexPendingOmissionDoesNotCountAheadOfHeadPage(t *testing.T) {
	raw := strings.Repeat("[a/s] line\n", 10000) + "[a/s] " + strings.Repeat("x", maxFilterLineBytes+1)
	store := runningLog(t, "pending", raw)
	body, meta, err := store.ReadActiveFiltered("pending", "a", "s", Filter{Limit: 1})
	if err != nil || string(body) != "[a/s] line\n" || !meta.HasMore || meta.OmittedLongLines != 0 {
		t.Fatalf("head page counted a distant pending record: %q %+v %v", body, meta, err)
	}
}

func TestRunningIndexConcurrentGlobalBudget(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	raw := strings.Repeat("[a/s0] x\n[a/s1] y\n", maxRunningRanges/2+1)
	var entries []*runningIndex
	for i := range 12 {
		entries = append(entries, store.pinRunningIndex(fmt.Sprintf("concurrent-%d", i)))
	}
	var wg sync.WaitGroup
	errs := make(chan error, len(entries))
	for _, entry := range entries {
		wg.Go(func() {
			_, err := entry.advance(t.Context(), strings.NewReader(raw), int64(len(raw)))
			if err != nil {
				errs <- err
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if store.runningSlots > maxRunningRangeSlots {
		t.Fatalf("concurrent reservation exceeded global budget: %d", store.runningSlots)
	}
	for _, entry := range entries {
		if entry.position != int64(len(raw)) {
			t.Fatal("global pressure stopped a writer's index")
		}
		store.unpinRunningIndex(entry)
	}
	for id := range store.running {
		store.ForgetActiveIndex(id)
	}
	if store.runningSlots != 0 || store.runningEntries != 0 {
		t.Fatal("concurrent reservations leaked")
	}
}

func TestBoundedShortUnterminatedRangeIsNotOversized(t *testing.T) {
	raw := "[a/s] final without newline"
	body, meta, err := scanBoundedRanges(strings.NewReader(raw), []Range{{Start: 0, End: int64(len(raw))}}, int64(len(raw)), Filter{})
	if err != nil || string(body) != raw || meta.OmittedLongLines != 0 || meta.ReturnedLines != 1 {
		t.Fatalf("small range buffer misclassified EOF: %q %+v %v", body, meta, err)
	}
}

func TestRunningIndexInterleavedSnapshotPaging(t *testing.T) {
	var raw, expected strings.Builder
	for i := range 5000 {
		line := fmt.Sprintf("[a/s] match %04d %s\n", i, strings.Repeat("x", 900))
		raw.WriteString(line)
		raw.WriteString("[b/s] unrelated\n")
		expected.WriteString(line)
	}
	for _, tail := range []bool{false, true} {
		t.Run(fmt.Sprintf("tail-%t", tail), func(t *testing.T) {
			store := runningLog(t, "pages", raw.String())
			for range 4 {
				if _, _, err := store.ReadActiveFiltered("pages", "a", "s", Filter{Tail: true, Limit: 1}); err != nil {
					t.Fatal(err)
				}
			}
			var cursor *int64
			var pages []string
			finished := false
			for range 20 {
				body, meta, err := store.ReadActiveFiltered("pages", "a", "s", Filter{Tail: tail, Pattern: "match", Limit: 10000, ByteOffset: cursor})
				if err != nil {
					t.Fatal(err)
				}
				if meta.DiscoveryBytes != 0 || meta.ScannedBytes > MaxScanBytes {
					t.Fatalf("warm interleaved page exceeded bounds: %+v", meta)
				}
				pages = append(pages, string(body))
				if !meta.HasMore {
					finished = true
					break
				}
				before := int64(0)
				if tail {
					before = int64(raw.Len())
				}
				if cursor != nil {
					before = *cursor
				}
				if (tail && meta.NextByteOffset >= before) || (!tail && meta.NextByteOffset <= before) {
					t.Fatalf("interleaved cursor stalled: %+v", meta)
				}
				cursor = &meta.NextByteOffset
			}
			if tail {
				slices.Reverse(pages)
			}
			if !finished || strings.Join(pages, "") != expected.String() {
				t.Fatalf("snapshot paging lost/duplicated lines: finished=%v pages=%d bytes=%d want=%d", finished, len(pages), len(strings.Join(pages, "")), expected.Len())
			}
		})
	}
}
