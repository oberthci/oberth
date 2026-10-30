package runlog

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/oberthci/oberth/internal/runprogress"
)

const (
	maxRunningIndexes = 64
	// Per-run suffix limit and shared allocated capacity; neither makes overflow sticky.
	maxRunningRanges           = 64 << 10
	maxRunningRangeSlots       = 512 << 10
	initialRunningRanges       = 64
	maxRunningLabels           = 4096
	maxRunningLabelBytes       = 128 << 10
	runningIndexAdvance  int64 = MaxScanBytes / 2
)

type runningLabel struct {
	burn, step string
}

type runningLabelRef struct {
	key   runningLabel
	refs  int
	bytes int64
}

// Labels live once in the intern table. Each range uses two int64 offsets and
// a parallel uint16 label ID (18 bytes of payload instead of Range's 48).
type runningSpan struct{ start, end int64 }

type runningIndex struct {
	gate                chan struct{}
	refs                int // refs, used, charged and detached are protected by runningMu.
	used                uint64
	charged             int
	detached            bool
	store               *Store
	info                os.FileInfo
	position, lineStart int64
	floor               int64 // Older indexed coverage was discarded; raw-file paging still works.
	prefix              []byte
	ranges              []runningSpan
	rangeLabels         []uint16
	first, count        int
	labels              []runningLabelRef
	labelIDs            map[runningLabel]uint16
	freeLabels          []uint16
	labelBytes          int
	active              bool
}

// Called only under runningMu, with no readers left on this entry.
func (store *Store) releaseRunningIndex(entry *runningIndex) {
	store.runningEntries--
	store.runningSlots -= entry.charged
	entry.charged = 0
	entry.ranges, entry.rangeLabels = nil, nil
	entry.labels, entry.labelIDs, entry.freeLabels, entry.prefix = nil, nil, nil, nil
	entry.count = 0
}

func (store *Store) evictRunningIndex(except *runningIndex) bool {
	victim := ""
	var oldest uint64
	for id, candidate := range store.running {
		if candidate != except && candidate.refs == 0 && (victim == "" || candidate.used < oldest) {
			victim, oldest = id, candidate.used
		}
	}
	if victim == "" {
		return false
	}
	entry := store.running[victim]
	delete(store.running, victim)
	store.releaseRunningIndex(entry)
	return true
}

func (store *Store) pinRunningIndex(runID string) *runningIndex {
	store.runningMu.Lock()
	defer store.runningMu.Unlock()
	if store.running == nil {
		store.running = make(map[string]*runningIndex)
	}
	entry := store.running[runID]
	if entry == nil {
		for store.runningEntries >= maxRunningIndexes || store.runningSlots+initialRunningRanges > maxRunningRangeSlots {
			if !store.evictRunningIndex(nil) {
				return nil
			}
		}
		entry = &runningIndex{gate: make(chan struct{}, 1), store: store, charged: initialRunningRanges,
			ranges: make([]runningSpan, initialRunningRanges), rangeLabels: make([]uint16, initialRunningRanges)}
		store.runningEntries++
		store.runningSlots += entry.charged
		store.running[strings.Clone(runID)] = entry
	}
	store.runningClock++
	entry.used = store.runningClock
	entry.refs++
	return entry
}

func (store *Store) unpinRunningIndex(entry *runningIndex) {
	store.runningMu.Lock()
	defer store.runningMu.Unlock()
	entry.refs--
	if entry.refs == 0 && entry.detached {
		store.releaseRunningIndex(entry)
	}
}

// ForgetActiveIndex releases retained state without waiting for a log reader.
// Detached, in-flight entries remain charged until their last reader leaves.
func (store *Store) ForgetActiveIndex(runID string) {
	store.runningMu.Lock()
	defer store.runningMu.Unlock()
	if entry := store.running[runID]; entry != nil {
		delete(store.running, runID)
		entry.detached = true
		if entry.refs == 0 {
			store.releaseRunningIndex(entry)
		}
	}
}

func (entry *runningIndex) reset(info os.FileInfo) {
	entry.info = info
	entry.position, entry.lineStart, entry.floor = 0, 0, 0
	entry.first, entry.count = 0, 0
	entry.labels, entry.labelIDs, entry.freeLabels, entry.prefix = nil, nil, nil, nil
	entry.labelBytes = 0
	entry.active = false
}

func (entry *runningIndex) slot(i int) int { return (entry.first + i) % len(entry.ranges) }

func (entry *runningIndex) grow() bool {
	capacity := min(maxRunningRanges, max(initialRunningRanges, len(entry.ranges)*2))
	if capacity <= len(entry.ranges) {
		return false
	}
	if entry.store != nil {
		store := entry.store
		store.runningMu.Lock()
		for store.runningSlots+capacity-entry.charged > maxRunningRangeSlots {
			if !store.evictRunningIndex(entry) {
				store.runningMu.Unlock()
				return false
			}
		}
		store.runningSlots += capacity - entry.charged
		entry.charged = capacity
		store.runningMu.Unlock()
	}
	ranges := make([]runningSpan, capacity)
	labels := make([]uint16, capacity)
	for i := range entry.count {
		slot := entry.slot(i)
		ranges[i], labels[i] = entry.ranges[slot], entry.rangeLabels[slot]
	}
	entry.ranges, entry.rangeLabels, entry.first = ranges, labels, 0
	return true
}

func (entry *runningIndex) dropOldest(count int) {
	for range count {
		slot := entry.first
		span := entry.ranges[slot]
		id := entry.rangeLabels[slot]
		label := &entry.labels[id]
		label.refs--
		label.bytes -= span.end - span.start
		if label.refs == 0 {
			delete(entry.labelIDs, label.key)
			entry.labelBytes -= len(label.key.burn) + len(label.key.step)
			*label = runningLabelRef{}
			entry.freeLabels = append(entry.freeLabels, id)
		}
		entry.floor = max(entry.floor, span.end)
		entry.first = (entry.first + 1) % len(entry.ranges)
		entry.count--
	}
	if entry.count == 0 {
		entry.active = false
	}
}

func (entry *runningIndex) intern(burn, step string) uint16 {
	key := runningLabel{burn, step}
	if id, ok := entry.labelIDs[key]; ok {
		return id
	}
	for entry.labelBytes+len(burn)+len(step) > maxRunningLabelBytes || len(entry.labelIDs) == maxRunningLabels {
		entry.dropOldest(max(1, entry.count/2))
	}
	if entry.labelIDs == nil {
		entry.labelIDs = make(map[runningLabel]uint16)
	}
	key = runningLabel{strings.Clone(burn), strings.Clone(step)}
	var id uint16
	if n := len(entry.freeLabels); n > 0 {
		id = entry.freeLabels[n-1]
		entry.freeLabels = entry.freeLabels[:n-1]
		entry.labels[id] = runningLabelRef{key: key}
	} else {
		id = uint16(len(entry.labels)) //nolint:gosec // No free IDs means all slots are live; intern caps those at 4096.
		entry.labels = append(entry.labels, runningLabelRef{key: key})
	}
	entry.labelIDs[key] = id
	entry.labelBytes += len(burn) + len(step)
	return id
}

func (entry *runningIndex) appendPrefix(fragment []byte) {
	wanted := min(maxMarkerBytes, len(entry.prefix)+len(fragment))
	if wanted > cap(entry.prefix) {
		capacity := min(maxMarkerBytes, max(wanted, max(64, cap(entry.prefix)*2)))
		prefix := make([]byte, len(entry.prefix), capacity)
		copy(prefix, entry.prefix)
		entry.prefix = prefix
	}
	entry.prefix = append(entry.prefix, fragment[:wanted-len(entry.prefix)]...)
}

func (entry *runningIndex) completeLine() {
	if bytes.HasPrefix(entry.prefix, []byte(runprogress.LogDeliveryPrefix)) {
		entry.active = false
	}
	burn, step, marked := ParseMarker(entry.prefix)
	if marked {
		same := false
		if entry.active {
			last := entry.slot(entry.count - 1)
			key := entry.labels[entry.rangeLabels[last]].key
			same = key.burn == burn && key.step == step
		}
		if !same {
			if entry.count == len(entry.ranges) && !entry.grow() {
				entry.dropOldest(max(1, entry.count/2))
			}
			id := entry.intern(burn, step)
			slot := entry.slot(entry.count)
			entry.ranges[slot] = runningSpan{start: entry.lineStart, end: entry.lineStart}
			entry.rangeLabels[slot] = id
			entry.labels[id].refs++
			entry.count++
		}
		entry.active = true
	}
	if entry.active {
		slot := entry.slot(entry.count - 1)
		span := &entry.ranges[slot]
		entry.labels[entry.rangeLabels[slot]].bytes += entry.position - span.end
		span.end = entry.position
	}
	entry.lineStart = entry.position
	entry.prefix = entry.prefix[:0]
}

// advance consumes only bytes after position. Incomplete records carry at most
// their marker prefix across calls, so giant records do not cause rereads.
func (entry *runningIndex) advance(ctx context.Context, file io.ReaderAt, size int64) (int64, error) {
	end := min(size, entry.position+runningIndexAdvance)
	if entry.position >= end {
		return 0, ctx.Err()
	}
	var read int64
	buffer := make([]byte, 64<<10)
	for entry.position < end {
		if err := ctx.Err(); err != nil {
			return read, err
		}
		n, err := file.ReadAt(buffer[:min(int64(len(buffer)), end-entry.position)], entry.position)
		read += int64(n)
		if n == 0 {
			if err == nil {
				err = io.ErrNoProgress
			}
			return read, err
		}
		fragment := buffer[:n]
		for len(fragment) > 0 {
			newline := bytes.IndexByte(fragment, '\n')
			take := len(fragment)
			if newline >= 0 {
				take = newline + 1
			}
			entry.appendPrefix(fragment[:take])
			entry.position += int64(take)
			if newline >= 0 {
				entry.completeLine()
			}
			fragment = fragment[take:]
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return read, err
		}
	}
	return read, nil
}

// snapshot copies at most a filtering budget of matching ranges. Older/newer
// coverage outside the snapshot is advertised separately for byte continuation.
func (entry *runningIndex) snapshot(burn, step string, filter Filter) (selected []Range, total int64, earlier, later bool) {
	matches := 0
	for _, label := range entry.labels {
		if label.refs > 0 && label.key.burn == burn && (step == "" || label.key.step == step) {
			total += label.bytes
			matches += label.refs
		}
	}
	// Start small: a clipped cursor may select few of the retained matches.
	selected = make([]Range, 0, min(matches, 1024))
	var bytes int64
	for n := range entry.count {
		i := n
		if filter.Tail {
			i = entry.count - 1 - n
		}
		slot := entry.slot(i)
		span, key := entry.ranges[slot], entry.labels[entry.rangeLabels[slot]].key
		if key.burn != burn || (step != "" && key.step != step) {
			continue
		}
		if filter.ByteOffset != nil {
			if filter.Tail && span.start >= *filter.ByteOffset {
				continue
			}
			if !filter.Tail && span.end <= *filter.ByteOffset {
				continue
			}
		}
		if bytes >= MaxScanBytes {
			if filter.Tail {
				earlier = true
			} else {
				later = true
			}
			break
		}
		selected = append(selected, Range{Burn: key.burn, Step: key.step, Start: span.start, End: span.end})
		start, end := span.start, span.end
		if filter.ByteOffset != nil {
			if filter.Tail {
				end = min(end, *filter.ByteOffset)
			} else {
				start = max(start, *filter.ByteOffset)
			}
		}
		bytes += end - start
	}
	if filter.Tail {
		slices.Reverse(selected)
	}
	return selected, total, earlier, later
}

// terminalIndex recognizes a published terminal index. Rechecking after cache
// admission closes the race with BuildIndex dropping a previously active entry.
func (store *Store) terminalIndex(ctx context.Context, runID string, info os.FileInfo) (*Index, error) {
	path, err := store.indexPath(runID)
	if err != nil {
		return nil, err
	}
	indexInfo, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	if indexInfo.ModTime().Before(info.ModTime()) {
		return nil, nil
	}
	index, err := store.loadIndexContext(ctx, runID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	if index.Size == info.Size() {
		return &index, nil
	}
	return nil, nil
}

func (store *Store) readActiveIndexed(runID, burn, step string, filter Filter) ([]byte, Meta, error) {
	ctx := filter.ctx()
	if err := ctx.Err(); err != nil {
		return nil, Meta{}, err
	}
	path, err := store.logPath(runID)
	if err != nil {
		return nil, Meta{}, err
	}
	file, err := os.Open(path) //nolint:gosec // logPath confines the validated run ID.
	if err != nil {
		store.ForgetActiveIndex(runID)
		return nil, Meta{}, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, Meta{}, err
	}
	terminal, err := store.terminalIndex(ctx, runID, info)
	if err != nil {
		return nil, Meta{}, err
	}
	if terminal != nil {
		store.ForgetActiveIndex(runID)
		return filterPublishedIndex(file, *terminal, burn, step, filter)
	}
	entry := store.pinRunningIndex(runID)
	if entry == nil {
		return store.readActiveWindow(runID, burn, step, filter, MaxScanBytes)
	}
	defer store.unpinRunningIndex(entry)
	select {
	case entry.gate <- struct{}{}:
	case <-ctx.Done():
		return nil, Meta{}, ctx.Err()
	}
	locked := true
	unlock := func() {
		if locked {
			<-entry.gate
			locked = false
		}
	}
	defer unlock()
	info, err = file.Stat()
	if err != nil {
		return nil, Meta{}, err
	}
	terminal, err = store.terminalIndex(ctx, runID, info)
	if err != nil {
		return nil, Meta{}, err
	}
	if terminal != nil {
		store.ForgetActiveIndex(runID)
		unlock()
		return filterPublishedIndex(file, *terminal, burn, step, filter)
	}
	if entry.info == nil || !os.SameFile(entry.info, info) || info.Size() < entry.info.Size() || (info.Size() == entry.info.Size() && !info.ModTime().Equal(entry.info.ModTime())) {
		entry.reset(info)
	}
	var discovered int64
	if entry.lineStart > 0 && info.Size() > entry.info.Size() {
		var previous [1]byte
		if _, err := file.ReadAt(previous[:], entry.lineStart-1); err != nil {
			return nil, Meta{}, err
		}
		// Catch truncate/regrow that passes the size/inode checks. Production is
		// append-only; checking this boundary is a cheap corruption guard, not a hash.
		if previous[0] != '\n' {
			entry.reset(info)
		}
		discovered++
	}
	entry.info = info
	advanced, err := entry.advance(ctx, file, info.Size())
	discovered += advanced
	if err != nil {
		return nil, Meta{}, err
	}
	position, complete := entry.position, entry.lineStart
	caughtUp := position == info.Size()
	cursor := int64(0)
	if filter.ByteOffset != nil {
		cursor = *filter.ByteOffset
	}
	covered := (!filter.Tail && cursor >= entry.floor) || (filter.Tail && (filter.ByteOffset == nil || cursor > entry.floor))
	usable := covered && (caughtUp || (!filter.Tail && cursor <= complete) || (filter.Tail && filter.ByteOffset != nil && cursor <= complete))
	if !usable {
		unlock()
		body, meta, err := store.readActiveWindow(runID, burn, step, filter, MaxScanBytes-discovered)
		meta.DiscoveryBytes += discovered
		return body, meta, err
	}
	selected, total, earlier, later := entry.snapshot(burn, step, filter)
	floor := entry.floor
	pendingOmitted := false
	if position-complete > maxFilterLineBytes {
		b, s, marked := ParseMarker(entry.prefix)
		if !marked && entry.active && !bytes.HasPrefix(entry.prefix, []byte(runprogress.LogDeliveryPrefix)) {
			last := entry.slot(entry.count - 1)
			key := entry.labels[entry.rangeLabels[last]].key
			b, s = key.burn, key.step
		}
		pendingOmitted = b == burn && (step == "" || s == step)
	}
	frontier := complete
	if position-complete > maxFilterLineBytes {
		frontier = position
	}
	exact := caughtUp && complete == position && floor == 0
	unlock()
	if total == 0 && caughtUp && floor == 0 && !pendingOmitted {
		return nil, Meta{}, fmt.Errorf("active log slice %s/%s: %w", burn, step, os.ErrNotExist)
	}
	body, meta, err := filterRanges(file, selected, total, filter)
	if err != nil {
		return nil, Meta{}, err
	}
	meta.DiscoveryBytes = discovered
	meta.BytesExact = exact
	meta.ScanComplete = meta.ScanComplete && exact
	if pendingOmitted && (filter.Tail || (!meta.HasMore && !later)) {
		meta.OmittedLongLines++
		meta.TotalLines++
		meta.WithheldLines++
	}
	if filter.Tail && (earlier || floor > 0) {
		if !meta.HasMore {
			meta.NextByteOffset = floor
			if len(selected) > 0 {
				meta.NextByteOffset = selected[0].Start
			}
		}
		meta.HasMore = true
	}
	if !filter.Tail && later {
		if !meta.HasMore && len(selected) > 0 {
			meta.NextByteOffset = selected[len(selected)-1].End
		}
		meta.HasMore = true
	}
	if !filter.Tail && !caughtUp {
		if !meta.HasMore {
			meta.NextByteOffset = max(cursor, frontier)
		}
		meta.HasMore = true
	}
	meta.Truncated = meta.Truncated || !exact || pendingOmitted
	return body, meta, nil
}

// Reuse the already-decoded published index when an active DB snapshot races
// terminalization; do not spend the index JSON budget a second time.
func filterPublishedIndex(file *os.File, index Index, burn, step string, filter Filter) ([]byte, Meta, error) {
	var selected []Range
	var total int64
	for _, r := range index.Ranges {
		if r.Burn == burn && (step == "" || r.Step == step) && r.End >= r.Start {
			selected = append(selected, r)
			total += r.End - r.Start
		}
	}
	if len(selected) == 0 {
		return nil, Meta{}, fmt.Errorf("log slice %s/%s: %w", burn, step, os.ErrNotExist)
	}
	return filterRanges(file, selected, total, filter)
}
