package apicontract

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"
)

// SSEFrame carries request-scoped message data; comments carry no data.
type SSEFrame struct {
	Data      []byte
	Keepalive bool
}

// ReadSSE bounds each frame and stops immediately when emit returns an error.
// Callers bounding a complete response also supply an io.LimitedReader.
func ReadSSE(reader io.Reader, limit int, emit func(SSEFrame) error) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, min(4096, limit+1)), limit+1)
	var frame SSEFrame
	size := 0
	for scanner.Scan() {
		line := scanner.Text()
		size += len(line) + 1
		if size > limit {
			return ErrReplySize
		}
		if line == "" {
			frame.Data = bytes.TrimSuffix(frame.Data, []byte{'\n'})
			if err := emit(frame); err != nil {
				return err
			}
			frame, size = SSEFrame{}, 0
			continue
		}
		frame.line(line)
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return ErrReplySize
		}
		return replyError(err)
	}
	// A truncated frame is not delivered or acknowledged by its cursor.
	if size > 0 {
		return io.ErrUnexpectedEOF
	}
	return nil
}

func (f *SSEFrame) line(line string) {
	field, value, _ := strings.Cut(line, ":")
	value = strings.TrimPrefix(value, " ")
	switch field {
	case "data":
		f.Data = append(f.Data, value...)
		f.Data = append(f.Data, '\n')
	case "":
		f.Keepalive = f.Keepalive || value == "keepalive"
	}
}
