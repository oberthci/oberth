package runlog

import (
	"bytes"
	"context"
	"io"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/oberthci/oberth/internal/publicerr"
)

type record struct {
	offset int64
	lo, hi uint32
	number uint32
}

func pattern(f Filter) (*regexp.Regexp, error) {
	if f.Context < 0 || f.Context > 50 || f.Offset < 0 || f.Limit < 0 || (f.ByteOffset != nil && *f.ByteOffset < 0) {
		return nil, publicerr.New(publicerr.InvalidInput, invalidLogWindowReason, windowRemedy, nil)
	}
	if len(f.Pattern) > 512 {
		return nil, publicerr.New(publicerr.InvalidInput, logPatternExceeds512BytesReason, patternLimitRemedy, nil)
	}
	if f.Pattern == "" {
		return nil, nil
	}
	compiled, err := regexp.Compile(f.Pattern)
	if err != nil {
		return nil, publicerr.New(publicerr.InvalidInput, invalidLogRegularExpressionReason, patternRemedy, err)
	}
	return compiled, nil
}
func scan(ctx context.Context, source io.ReaderAt, spans []span, f Filter) ([]byte, Meta, error) {
	meta := Meta{ScanLimitBytes: MaxScanBytes, BytesExact: true, Tail: f.Tail}
	compiled, err := pattern(f)
	if err != nil {
		return nil, meta, err
	}
	if err = ctx.Err(); err != nil {
		return nil, meta, storage(logOperationCanceledReason, err)
	}
	for _, part := range spans {
		meta.Bytes += part.end - part.start
	}
	parts := windows(spans, f)
	if len(parts) == 0 {
		meta.ScanComplete = meta.Bytes == 0
		if f.ByteOffset != nil {
			meta.NextByteOffset = *f.ByteOffset
		}
		return []byte{}, meta, nil
	}
	data, scanned, err := readWindow(ctx, source, parts)
	meta.ScannedBytes = scanned
	if err != nil {
		return nil, meta, err
	}
	view := lineWindow{data: data, lines: records(data, parts, &meta)}
	meta.NextByteOffset = windowCursor(data, parts, f.Tail)
	meta.MatchedLines, err = view.match(ctx, compiled, f.Context)
	if err != nil {
		return nil, meta, err
	}
	meta.ScanComplete = int64(len(data)) == meta.Bytes
	meta.TotalLines = len(view.lines) + meta.OmittedLongLines
	body := view.page(f, &meta)
	finishWindow(spans, f, &meta)
	return body, meta, nil
}
func finishWindow(spans []span, f Filter, meta *Meta) {
	if !meta.ScanComplete {
		meta.TotalLines, meta.MatchedLines = 0, 0
	}
	meta.Truncated = meta.Truncated || !meta.ScanComplete || meta.WithheldLines > 0 || meta.OmittedLongLines > 0
	if f.Tail {
		meta.HasMore = meta.NextByteOffset > spans[0].start
	} else {
		meta.HasMore = meta.NextByteOffset < spans[len(spans)-1].end
	}

	meta.Truncated = meta.Truncated || meta.HasMore
}

// windowCursor revisits a bounded edge fragment on the next page. A fragment
// already longer than a record, or with no known beginning, advances instead.
func windowCursor(data []byte, parts []window, tail bool) int64 {
	if tail {
		part := parts[0]
		if part.partial {
			if end := bytes.IndexByte(data[part.lo:part.hi], '\n'); end >= 0 && end+1 <= maxRecordBytes {
				return part.bounds.start + int64(end) + 1
			}
		}
		return part.bounds.start
	}
	part := parts[len(parts)-1]
	block := data[part.lo:part.hi]
	start := bytes.LastIndexByte(block, '\n') + 1
	shortFragment := len(block)-start <= maxRecordBytes
	knownStart := start > 0 || !part.partial
	if part.bounds.end < part.whole.end && shortFragment && knownStart {
		return part.bounds.start + int64(start)
	}
	return part.bounds.end
}

func records(data []byte, parts []window, meta *Meta) []record {
	var lines []record
	var number uint32
	for _, part := range parts {
		for lo := part.lo; lo < part.hi; {
			number++
			end := part.hi
			if at := bytes.IndexByte(data[lo:part.hi], '\n'); at >= 0 {
				end = lo + uint32(at) + 1
			}
			partial := lo == part.lo && part.partial || end == part.hi && data[end-1] != '\n' && part.bounds.end < part.whole.end
			if partial || end-lo > maxRecordBytes {
				meta.WithheldBytes += int64(end - lo)
				meta.WithheldLines++
				if end-lo > maxRecordBytes {
					meta.OmittedLongLines++
				}
			} else {
				lines = append(lines, record{part.bounds.start + int64(lo-part.lo), lo, end, number})
			}
			lo = end
		}
	}
	return lines
}

type lineWindow struct {
	data     []byte
	lines    []record
	selected []bool
}

func (v *lineWindow) page(f Filter, meta *Meta) []byte {
	chosen := v.pageIndices(f)
	var bodies [][]byte
	size := 0
	capped := false
	for _, index := range chosen {
		line := v.lines[index]
		text := v.data[line.lo:line.hi]
		if !utf8.Valid(text) {
			text = []byte(strings.ToValidUTF8(string(text), "�"))
			meta.BytesExact = false
		}
		if capped || size+len(text) > int(MaxScanBytes) {
			capped = true
			meta.WithheldLines++
			meta.WithheldBytes += int64(line.hi - line.lo)
			continue
		}
		bodies = append(bodies, text)
		size += len(text)
		meta.LineOffsets = append(meta.LineOffsets, line.offset)
		if meta.ScanComplete {
			meta.LineNumbers = append(meta.LineNumbers, int(line.number))
		}
		meta.ReturnedLines++
		if f.Tail {
			meta.NextByteOffset = line.offset
		} else {
			meta.NextByteOffset = line.offset + int64(line.hi-line.lo)
		}
	}
	if f.Tail {
		slices.Reverse(bodies)
		slices.Reverse(meta.LineOffsets)
		slices.Reverse(meta.LineNumbers)
	}
	return bytes.Join(bodies, nil)
}

func (v *lineWindow) pageIndices(f Filter) []uint32 {
	indices := make([]uint32, 0)
	for i, yes := range v.selected {
		if yes {
			indices = append(indices, uint32(i))
		}
	}
	start, end := min(f.Offset, len(indices)), len(indices)
	if f.Tail {
		end = max(0, end-f.Offset)
		start = 0
		if f.Limit > 0 {
			start = max(0, end-f.Limit)
		}
	} else if f.Limit > 0 {
		end = start + min(end-start, f.Limit)
	}
	chosen := indices[start:end]
	if f.Tail {
		slices.Reverse(chosen)
	}
	return chosen
}

func (v *lineWindow) match(ctx context.Context, pattern *regexp.Regexp, contextLines int) (int, error) {
	v.selected = make([]bool, len(v.lines))
	matched := 0
	for i, line := range v.lines {
		if err := ctx.Err(); err != nil {
			return 0, storage(logOperationCanceledReason, err)
		}
		if pattern != nil && !pattern.Match(bytes.TrimSuffix(v.data[line.lo:line.hi], []byte("\n"))) {
			continue
		}
		matched++
		for n := max(0, i-contextLines); n <= min(len(v.lines)-1, i+contextLines); n++ {
			v.selected[n] = true
		}
	}
	return matched, nil
}
