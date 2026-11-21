package runlog

import (
	"context"
	"errors"
	"io"
	"slices"
)

type window struct {
	whole, bounds span
	lo, hi        uint32
	partial       bool
}

func windows(spans []span, f Filter) []window {
	remaining := MaxScanBytes - 1
	var result []window
	visit := func(whole span) {
		part := whole
		if f.ByteOffset != nil {
			if f.Tail {
				part.end = min(part.end, *f.ByteOffset)
			} else {
				part.start = max(part.start, *f.ByteOffset)
			}
		}
		if part.end <= part.start || remaining == 0 {
			return
		}
		if f.Tail {
			part.start = max(part.start, part.end-remaining)
		} else {
			part.end = min(part.end, part.start+remaining)
		}
		remaining -= part.end - part.start
		result = append(result, window{whole: whole, bounds: part})
	}
	if f.Tail {
		for i := len(spans) - 1; i >= 0; i-- {
			visit(spans[i])
		}
		slices.Reverse(result)
	} else {
		for _, part := range spans {
			visit(part)
		}
	}
	return result
}
func readWindow(ctx context.Context, source io.ReaderAt, parts []window) ([]byte, int64, error) {
	var data []byte
	var scanned int64
	for i := range parts {
		part := &parts[i]
		part.lo = uint32(len(data))
		if part.bounds.start > part.whole.start {
			var previous [1]byte
			n, err := source.ReadAt(previous[:], part.bounds.start-1)
			scanned += int64(n)
			if err != nil {
				return nil, scanned, storage(cannotReadLogRecordBoundaryReason, err)
			}
			part.partial = previous[0] != '\n'
		}
		wanted := part.bounds.end - part.bounds.start
		for wanted > 0 {
			if err := ctx.Err(); err != nil {
				return nil, scanned, storage(logOperationCanceledReason, err)
			}
			block := make([]byte, min(wanted, 32<<10))
			n, err := source.ReadAt(block, part.bounds.start+int64(len(data))-int64(part.lo))
			data = append(data, block[:n]...)
			scanned += int64(n)
			wanted -= int64(n)
			if err != nil || n == 0 {
				if errors.Is(err, io.EOF) {
					err = io.ErrUnexpectedEOF
				}
				return nil, scanned, storage(retainedLogBytesAreUnavailableReason, err)
			}
		}
		part.hi = uint32(len(data))
	}
	return data, scanned, nil
}
