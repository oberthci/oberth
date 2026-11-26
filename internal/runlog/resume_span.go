package runlog

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
)

// Resolve the boundary before filtering, tailing or pagination. A missing
// marker never exposes the successor's output through the predecessor.
func beforeMarker(ctx context.Context, source io.ReaderAt, spans []span, marker string) ([]span, error) {
	var prefix []span
	for _, part := range spans {
		bounds, found, err := markerOffset(ctx, io.NewSectionReader(source, part.start, part.end-part.start), part.start, marker)
		if err != nil {
			return nil, err
		}
		if found {
			prefix = append(prefix, span{part.start, bounds.start})
			return prefix, nil
		}
		prefix = append(prefix, part)
	}
	return nil, nil
}

func afterMarker(ctx context.Context, source io.ReaderAt, spans []span, marker string) ([]span, error) {
	for i, part := range spans {
		bounds, found, err := markerOffset(ctx, io.NewSectionReader(source, part.start, part.end-part.start), part.start, marker)
		if err != nil {
			return nil, err
		}
		if found {
			return append([]span{{bounds.end, part.end}}, spans[i+1:]...), nil
		}
	}
	return nil, nil
}

func markerOffset(ctx context.Context, input io.Reader, start int64, marker string) (span, bool, error) {
	reader := bufio.NewReaderSize(input, 64<<10)
	offset, foundAt := start, start
	var previous []byte
	matched := false
	for {
		if err := ctx.Err(); err != nil {
			return span{}, false, err
		}
		chunk, err := reader.ReadSlice('\n')
		data := append(previous, chunk...)
		if !matched {
			if at := bytes.Index(data, []byte(marker)); at >= 0 {
				foundAt = offset - int64(len(previous)) + int64(at)
				matched = true
			}
		}
		offset += int64(len(chunk))
		if errors.Is(err, bufio.ErrBufferFull) {
			keep := min(len(data), len(marker))
			previous = append([]byte(nil), data[len(data)-keep:]...)
			continue
		}
		if matched {
			return span{foundAt, offset}, true, nil
		}
		previous = nil
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return span{}, false, err
		}
	}
	return span{}, false, nil
}
