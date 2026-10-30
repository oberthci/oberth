package runlog

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
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
	if err != nil || string(body) != "[wanted/step] new bytes\n" || meta.DiscoveryBytes != int64(len("[wanted/step] new bytes\n")) {
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
	if !reflect.DeepEqual(entry.ranges, index.Ranges) {
		t.Fatalf("incremental attribution differs: %+v / %+v", entry.ranges, index.Ranges)
	}
	store := runningLog(t, "partial", "[a/s] first\n[b/u] 界")
	_, _, err = store.ReadActiveFiltered("partial", "a", "s", Filter{})
	if err != nil {
		t.Fatal(err)
	}
	appendRunning(t, store, "partial", " last\n")
	body, meta, err := store.ReadActiveFiltered("partial", "b", "u", Filter{})
	if err != nil || string(body) != "[b/u] 界 last\n" || meta.DiscoveryBytes != int64(len(" last\n")) {
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
	var fragmented strings.Builder
	for i := 0; i < maxRunningRanges+10; i++ {
		fmt.Fprintf(&fragmented, "[a/s%d] line\n", i%2)
	}
	overflow := runningLog(t, "overflow", fragmented.String())
	body, meta, err := overflow.ReadActiveFiltered("overflow", "a", "s0", Filter{Limit: 1})
	entry := overflow.running["overflow"]
	if err != nil || len(body) == 0 || !entry.overflow || len(entry.ranges) != 0 || cap(entry.prefix) != 0 || meta.DiscoveryBytes > MaxScanBytes {
		t.Fatalf("range cap/fallback failed: %+v %v", meta, err)
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
	body, meta, err := store.ReadActiveFiltered("labels", "wanted", "s", Filter{})
	entry := store.running["labels"]
	if err != nil || string(body) != "[wanted/s] final\n" || !entry.overflow || entry.labelBytes != 0 || len(entry.ranges) != 0 || meta.DiscoveryBytes > MaxScanBytes {
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
