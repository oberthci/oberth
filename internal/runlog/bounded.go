package runlog

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
)

const (
	// Discovery and filtering have separate byte budgets. Active reads perform
	// at most two bounded passes, independent of the total retained run size.
	MaxScanBytes int64 = 4 << 20
	// Active reads can spend one bounded pass discovering ranges and one
	// filtering them. This is the aggregate per-call bound, not a deadline.
	MaxCallScanBytes   int64 = 2 * MaxScanBytes
	initialTailBytes   int64 = 64 << 10
	maxFilterLineBytes       = 64 << 10
)

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func filterRanges(file *os.File, ranges []Range, total int64, filter Filter) ([]byte, Meta, error) {
	return scanBoundedRanges(file, ranges, total, filter)
}

func clippedRanges(ranges []Range, filter Filter) []Range {
	selected := make([]Range, 0, len(ranges))
	for _, r := range ranges {
		if filter.ByteOffset != nil {
			if filter.Tail {
				r.End = min(r.End, *filter.ByteOffset)
			} else {
				r.Start = max(r.Start, *filter.ByteOffset)
			}
		}
		if r.End > r.Start {
			selected = append(selected, r)
		}
	}
	return selected
}

func rangeBytes(ranges []Range) int64 {
	var total int64
	for _, r := range ranges {
		total += max(int64(0), r.End-r.Start)
	}
	return total
}

func rangeWindow(ranges []Range, maximum int64, tail bool) []Range {
	selected := append([]Range(nil), ranges...)
	remaining := maximum
	if tail {
		for i := len(selected) - 1; i >= 0; i-- {
			selected[i].Start = max(selected[i].Start, selected[i].End-remaining)
			remaining -= selected[i].End - selected[i].Start
			if remaining == 0 {
				return selected[i:]
			}
		}
	} else {
		for i := range selected {
			selected[i].End = min(selected[i].End, selected[i].Start+remaining)
			remaining -= selected[i].End - selected[i].Start
			if remaining == 0 {
				return selected[:i+1]
			}
		}
	}
	return selected
}

func scanBoundedRanges(file io.ReaderAt, ranges []Range, total int64, filter Filter) ([]byte, Meta, error) {
	if err := filter.ctx().Err(); err != nil {
		return nil, Meta{}, err
	}
	if filter.Context < 0 || filter.Context > maxContextLines || filter.Offset < 0 || filter.Limit < 0 || (filter.ByteOffset != nil && *filter.ByteOffset < 0) {
		return nil, Meta{}, fmt.Errorf("%w: invalid log page", ErrInvalidPattern)
	}
	pattern, err := compilePattern(filter.Pattern)
	if err != nil {
		return nil, Meta{}, err
	}
	candidates := clippedRanges(ranges, filter)
	remaining, window := MaxScanBytes, MaxScanBytes
	if filter.Tail && filter.Limit > 0 {
		window = initialTailBytes
	}
	var body []byte
	var meta Meta
	var scanned int64
	for {
		selected := rangeWindow(candidates, min(window, max(int64(0), remaining-1)), filter.Tail)
		collector := newCollector(pattern, filter)
		meta = Meta{Bytes: total, BytesExact: true}
		first := true
		for _, r := range selected {
			if err := filter.ctx().Err(); err != nil {
				return nil, Meta{}, err
			}
			discard := false
			if r.Start > 0 && r.Start != originalStart(ranges, r) {
				var previous [1]byte
				if _, err := file.ReadAt(previous[:], r.Start-1); err != nil {
					return nil, Meta{}, err
				}
				meta.ScannedBytes++
				discard = previous[0] != '\n'
			}
			reader := bufio.NewReaderSize(contextReader{filter.ctx(), io.NewSectionReader(file, r.Start, r.End-r.Start)}, maxFilterLineBytes)
			position, oversized := r.Start, false
			wholeEnd := originalEnd(ranges, r)
			if first {
				meta.NextByteOffset = r.Start
			}
			for {
				if err := filter.ctx().Err(); err != nil {
					return nil, Meta{}, err
				}
				line, readErr := reader.ReadSlice('\n')
				position += int64(len(line))
				if errors.Is(readErr, bufio.ErrBufferFull) {
					oversized = true
					continue
				}
				if readErr != nil && !errors.Is(readErr, io.EOF) {
					return nil, Meta{}, readErr
				}
				if len(line) != 0 || oversized {
					if discard {
						discard = false
						if oversized {
							meta.TotalLines++
							meta.OmittedLongLines++
							collector.dropped = true
						}
						if first && filter.Tail && readErr == nil {
							meta.NextByteOffset = position
						}
					} else if oversized {
						meta.TotalLines++
						meta.OmittedLongLines++
						collector.dropped = true
					} else if readErr == nil || position == wholeEnd {
						meta.TotalLines++
						collector.offer(numberedLine{number: meta.TotalLines, text: string(line), byteOffset: position - int64(len(line))})
					}
					oversized = false
				}
				if !filter.Tail {
					meta.NextByteOffset = position
					if errors.Is(readErr, io.EOF) && len(line) > 0 && position != wholeEnd && len(line) < maxFilterLineBytes {
						meta.NextByteOffset = position - int64(len(line))
					}
				}
				// Full counts for small steps remain exact. Large head pages stop
				// as soon as their returned-line budget is satisfied.
				count := collector.lines
				if pattern == nil {
					count = len(collector.open)
				}
				if !filter.Tail && total > initialTailBytes && ((filter.Limit > 0 && count >= filter.Limit) || collector.retained >= maxReadBytes) {
					break
				}
				if readErr != nil {
					break
				}
			}
			// Include read-ahead in the I/O budget, not only consumed lines.
			meta.ScannedBytes += position - r.Start + int64(reader.Buffered())
			first = false
			count := collector.lines
			if pattern == nil {
				count = len(collector.open)
			}
			if !filter.Tail && total > initialTailBytes && ((filter.Limit > 0 && count >= filter.Limit) || collector.retained >= maxReadBytes) {
				break
			}
		}
		collector.close()
		var numbers []int
		var byteTruncation bool
		body, numbers, byteTruncation = collector.bytes(maxReadBytes)
		meta.LineNumbers, meta.ReturnedLines, meta.MatchedLines = numbers, len(numbers), collector.matched
		meta.LineOffsets = collector.byteOffsets(numbers)
		inspectedEnd := meta.NextByteOffset
		// Resume from the returned records, not the edge of a wider tail window:
		// otherwise matches evicted from that window would be skipped forever.
		if len(meta.LineOffsets) != 0 {
			if filter.Tail {
				meta.NextByteOffset = meta.LineOffsets[0]
			} else {
				lastStart := bytes.LastIndexByte(bytes.TrimSuffix(body, []byte("\n")), '\n') + 1
				meta.NextByteOffset = meta.LineOffsets[len(meta.LineOffsets)-1] + int64(len(body)-lastStart)
			}
		}
		if pattern == nil {
			meta.MatchedLines = meta.TotalLines
		}
		if len(candidates) != 0 {
			if filter.Tail {
				// A window entirely inside a giant record can discard right up
				// to the incoming cursor. Continue before that window instead.
				if len(selected) != 0 && meta.NextByteOffset >= candidates[len(candidates)-1].End {
					meta.NextByteOffset = selected[0].Start
				}
				meta.HasMore = meta.NextByteOffset > candidates[0].Start
			} else {
				meta.HasMore = meta.NextByteOffset < candidates[len(candidates)-1].End
			}
		}
		inspectedAll := rangeBytes(selected) == rangeBytes(candidates)
		if !filter.Tail && len(candidates) > 0 {
			inspectedAll = inspectedAll && inspectedEnd >= candidates[len(candidates)-1].End
		}
		meta.ScanComplete = inspectedAll && rangeBytes(candidates) == total && meta.OmittedLongLines == 0
		meta.WithheldLines, meta.WithheldBytes = max(0, meta.TotalLines-meta.ReturnedLines), max(int64(0), total-int64(len(body)))
		meta.Truncated = byteTruncation || collector.withheld() || !meta.ScanComplete || meta.OmittedLongLines > 0
		scanned += meta.ScannedBytes
		remaining -= meta.ScannedBytes
		// Tail pages expand geometrically only if they have too few matches.
		// Sparse/no-match patterns cannot exceed the same per-call byte cap.
		if !filter.Tail || meta.ScanComplete || !meta.HasMore || (filter.Limit > 0 && meta.ReturnedLines >= filter.Limit) || remaining <= window {
			break
		}
		window = min(window*2, remaining)
	}
	meta.ScannedBytes = scanned
	meta.TotalScannedBytes, meta.ScanLimitBytes = scanned, MaxScanBytes
	return body, meta, nil
}

func originalStart(ranges []Range, selected Range) int64 {
	i := sort.Search(len(ranges), func(i int) bool { return ranges[i].End > selected.Start })
	if i < len(ranges) {
		return ranges[i].Start
	}
	return selected.Start
}

func originalEnd(ranges []Range, selected Range) int64 {
	i := sort.Search(len(ranges), func(i int) bool { return ranges[i].End > selected.Start })
	if i < len(ranges) {
		return ranges[i].End
	}
	return selected.End
}

// Track complete records during discovery itself. A bounded lookbehind cannot
// find the last newline when the window ends deep inside a giant record.
type completeRecordReader struct {
	reader       io.Reader
	position     int64
	lastComplete int64
}

func (r *completeRecordReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if newline := bytes.LastIndexByte(p[:n], '\n'); newline >= 0 {
		r.lastComplete = r.position + int64(newline) + 1
	}
	r.position += int64(n)
	return n, err
}

// readActiveBounded discovers ranges only inside a bounded raw-log window.
// A missing step in a partial window is an incomplete observation, not proof
// that the step has no output. Continuation cursors are absolute file offsets.
func (store *Store) readActiveBounded(runID, burn, step string, filter Filter) ([]byte, Meta, error) {
	path, err := store.logPath(runID)
	if err != nil {
		return nil, Meta{}, err
	}
	file, err := os.Open(path) //nolint:gosec // logPath confines the validated run ID.
	if err != nil {
		return nil, Meta{}, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, Meta{}, err
	}
	start, end := int64(0), info.Size()
	if filter.ByteOffset != nil {
		if *filter.ByteOffset < 0 {
			return nil, Meta{}, ErrInvalidPattern
		}
		if filter.Tail {
			end = min(end, *filter.ByteOffset)
		} else {
			start = min(end, *filter.ByteOffset)
		}
	}
	if filter.Tail {
		start = max(start, end-(MaxScanBytes-initialTailBytes-1))
	} else {
		end = min(end, start+(MaxScanBytes-initialTailBytes-1))
	}
	reader := bufio.NewReaderSize(contextReader{filter.ctx(), io.NewSectionReader(file, start, end-start)}, maxFilterLineBytes)
	base := start
	discoveryOmitted := 0
	if start > 0 {
		var previous [1]byte
		if _, err := file.ReadAt(previous[:], start-1); err != nil {
			return nil, Meta{}, err
		}
		if previous[0] != '\n' {
			for {
				fragment, err := reader.ReadSlice('\n')
				base += int64(len(fragment))
				if err == nil || errors.Is(err, io.EOF) {
					break
				}
				if !errors.Is(err, bufio.ErrBufferFull) {
					return nil, Meta{}, err
				}
			}
		}
	}
	complete := &completeRecordReader{reader: contextReader{filter.ctx(), reader}}
	index, err := indexReader(runID, complete)
	if err != nil {
		return nil, Meta{}, err
	}
	discoveryBytes := end - start
	if start > 0 {
		discoveryBytes++
	}
	rawEnd := end
	if end < info.Size() {
		end = base + complete.lastComplete
	}
	ranges := make([]Range, 0)
	for _, r := range index.Ranges {
		if r.Burn == burn && r.Step == step {
			// Only count a discovery omission when its marker attributes it
			// to this step. An unmarked leading fragment has unknown ownership.
			if rawEnd < info.Size() && rawEnd-end > maxFilterLineBytes && r.End+base == rawEnd {
				discoveryOmitted++
			}
			r.Start += base
			r.End = min(end, r.End+base)
			if r.End > r.Start {
				ranges = append(ranges, r)
			}
		}
	}
	partial := start > 0 || end < info.Size()
	if len(ranges) == 0 && !partial {
		return nil, Meta{}, fmt.Errorf("active log slice %s/%s: %w", burn, step, os.ErrNotExist)
	}
	body, meta, err := filterRanges(file, ranges, rangeBytes(ranges), filter)
	if err != nil {
		return nil, Meta{}, err
	}
	meta.DiscoveryBytes = discoveryBytes
	meta.TotalScannedBytes, meta.ScanLimitBytes = meta.ScannedBytes+discoveryBytes, MaxCallScanBytes
	meta.OmittedLongLines += discoveryOmitted
	meta.TotalLines += discoveryOmitted
	meta.WithheldLines += discoveryOmitted
	meta.BytesExact, meta.ScanComplete = !partial, meta.ScanComplete && !partial
	if !meta.HasMore {
		if filter.Tail {
			meta.NextByteOffset, meta.HasMore = base, start > 0
			if meta.NextByteOffset >= rawEnd && start > 0 {
				meta.NextByteOffset = start
			}
		} else {
			meta.NextByteOffset = end
			if end <= start {
				meta.NextByteOffset = rawEnd
			}
			meta.HasMore = meta.NextByteOffset < info.Size()
		}
	}
	if filter.Tail && meta.HasMore && meta.NextByteOffset >= rawEnd {
		meta.NextByteOffset, meta.HasMore = start, start > 0
	}
	meta.Truncated = meta.Truncated || partial
	return body, meta, nil
}

// Byte identities let independent bounded tail/diagnostic windows be merged
// without mistaking window-relative line numbers for global step line numbers.
func (c *collector) byteOffsets(numbers []int) []int64 {
	offsets := make([]int64, 0, len(numbers))
	for _, group := range c.groups {
		for _, line := range group {
			if len(offsets) < len(numbers) && line.number == numbers[len(offsets)] {
				offsets = append(offsets, line.byteOffset)
			}
		}
	}
	return offsets
}
