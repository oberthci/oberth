package runlog

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/oberthci/oberth/internal/runprogress"
)

const (
	maxRunningIndexes          = 64
	maxRunningRanges           = 4096
	maxRunningLabelBytes       = 128 << 10
	runningIndexAdvance  int64 = MaxScanBytes / 2
)

type runningIndex struct {
	gate       chan struct{}
	refs       int
	used       uint64
	info       os.FileInfo
	position   int64
	lineStart  int64
	prefix     []byte
	ranges     []Range
	labelBytes int
	active     bool
	overflow   bool
}

func (store *Store) pinRunningIndex(runID string) *runningIndex {
	store.runningMu.Lock()
	defer store.runningMu.Unlock()
	if store.running == nil {
		store.running = make(map[string]*runningIndex)
	}
	entry := store.running[runID]
	if entry == nil {
		if len(store.running) >= maxRunningIndexes {
			victim := ""
			var oldest uint64
			for id, candidate := range store.running {
				if candidate.refs == 0 && (victim == "" || candidate.used < oldest) {
					victim, oldest = id, candidate.used
				}
			}
			if victim == "" {
				return nil
			}
			delete(store.running, victim)
		}
		entry = &runningIndex{gate: make(chan struct{}, 1)}
		store.running[strings.Clone(runID)] = entry
	}
	store.runningClock++
	entry.used = store.runningClock
	entry.refs++
	return entry
}

func (store *Store) unpinRunningIndex(entry *runningIndex) {
	store.runningMu.Lock()
	entry.refs--
	store.runningMu.Unlock()
}

// ForgetActiveIndex releases retained index state without waiting for a log
// reader. An already-running read owns its snapshot until that request ends.
func (store *Store) ForgetActiveIndex(runID string) {
	store.runningMu.Lock()
	delete(store.running, runID)
	store.runningMu.Unlock()
}

func (entry *runningIndex) reset(info os.FileInfo) {
	entry.info = info
	entry.position = 0
	entry.lineStart = 0
	entry.prefix = nil
	entry.ranges = nil
	entry.labelBytes = 0
	entry.active = false
	entry.overflow = false
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
		last := len(entry.ranges) - 1
		if !entry.active || entry.ranges[last].Burn != burn || entry.ranges[last].Step != step {
			if len(entry.ranges) == maxRunningRanges || entry.labelBytes+len(burn)+len(step) > maxRunningLabelBytes {
				entry.overflow = true
				entry.ranges = nil
				entry.prefix = nil
				entry.active = false
				entry.labelBytes = 0
				return
			}
			if len(entry.ranges) == cap(entry.ranges) {
				ranges := make([]Range, len(entry.ranges), min(maxRunningRanges, max(8, cap(entry.ranges)*2)))
				copy(ranges, entry.ranges)
				entry.ranges = ranges
			}
			entry.ranges = append(entry.ranges, Range{Burn: strings.Clone(burn), Step: strings.Clone(step), Start: entry.lineStart})
			entry.labelBytes += len(burn) + len(step)
		}
		entry.active = true
	}
	if entry.active {
		entry.ranges[len(entry.ranges)-1].End = entry.position
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
	for entry.position < end && !entry.overflow {
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
		for len(fragment) > 0 && !entry.overflow {
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
	entry.info = info
	var discovered int64
	if !entry.overflow {
		discovered, err = entry.advance(ctx, file, info.Size())
		if err != nil {
			return nil, Meta{}, err
		}
	}
	position, complete := entry.position, entry.lineStart
	caughtUp := position == info.Size()
	cursor := int64(0)
	if filter.ByteOffset != nil {
		cursor = *filter.ByteOffset
	}
	usable := !entry.overflow && (caughtUp || (!filter.Tail && cursor <= complete) || (filter.Tail && filter.ByteOffset != nil && cursor <= complete))
	if !usable {
		unlock()
		body, meta, err := store.readActiveWindow(runID, burn, step, filter, MaxScanBytes-discovered)
		meta.DiscoveryBytes += discovered
		return body, meta, err
	}
	var selected []Range
	for _, r := range entry.ranges {
		if r.Burn == burn && (step == "" || r.Step == step) {
			selected = append(selected, r)
		}
	}
	pendingOmitted := false
	if position-complete > maxFilterLineBytes {
		b, s, marked := ParseMarker(entry.prefix)
		if !marked && entry.active && !bytes.HasPrefix(entry.prefix, []byte(runprogress.LogDeliveryPrefix)) {
			last := entry.ranges[len(entry.ranges)-1]
			b, s = last.Burn, last.Step
		}
		pendingOmitted = b == burn && (step == "" || s == step)
	}
	frontier := complete
	if position-complete > maxFilterLineBytes {
		frontier = position
	}
	exact := caughtUp && complete == position
	unlock()
	if len(selected) == 0 && caughtUp && !pendingOmitted {
		return nil, Meta{}, fmt.Errorf("active log slice %s/%s: %w", burn, step, os.ErrNotExist)
	}
	body, meta, err := filterRanges(file, selected, rangeBytes(selected), filter)
	if err != nil {
		return nil, Meta{}, err
	}
	meta.DiscoveryBytes = discovered
	meta.BytesExact = exact
	meta.ScanComplete = meta.ScanComplete && exact
	if pendingOmitted {
		meta.OmittedLongLines++
		meta.TotalLines++
		meta.WithheldLines++
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
