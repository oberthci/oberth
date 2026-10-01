package service

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/oberthci/oberth/internal/runprogress"
)

// stepProgressLogWriter removes runner control records from the retained user
// log and journals them as they arrive. It buffers at most one bounded marker;
// ordinary long lines stream directly to the retained-log limiter.
type stepProgressLogWriter struct {
	destination io.Writer
	persist     func(runprogress.Event) error
	pending     []byte
	passthrough bool
	closed      bool
}

func newStepProgressLogWriter(destination io.Writer, persist func(runprogress.Event) error) *stepProgressLogWriter {
	return &stepProgressLogWriter{destination: destination, persist: persist}
}

func (writer *stepProgressLogWriter) Write(body []byte) (int, error) {
	if writer.closed {
		return 0, errors.New("step progress log writer is closed")
	}
	want := len(body)
	if err := writer.consume(body); err != nil {
		return 0, err
	}
	return want, nil
}

func (writer *stepProgressLogWriter) Close() error {
	if writer.closed {
		return nil
	}
	writer.closed = true
	if len(writer.pending) == 0 {
		return nil
	}
	line := writer.pending
	writer.pending = nil
	return writer.processLine(line)
}

func (writer *stepProgressLogWriter) consume(body []byte) error {
	for len(body) > 0 {
		if writer.passthrough {
			newline := bytes.IndexByte(body, '\n')
			if newline < 0 {
				return writeProgressBytes(writer.destination, body)
			}
			if err := writeProgressBytes(writer.destination, body[:newline+1]); err != nil {
				return err
			}
			body = body[newline+1:]
			writer.passthrough = false
			continue
		}

		newline := bytes.IndexByte(body, '\n')
		if newline >= 0 {
			writer.pending = append(writer.pending, body[:newline+1]...)
			if err := writer.processLine(writer.pending); err != nil {
				return err
			}
			writer.pending = writer.pending[:0]
			body = body[newline+1:]
			continue
		}

		writer.pending = append(writer.pending, body...)
		if len(writer.pending) <= runprogress.MaxMarkerBytes {
			return nil
		}
		if bytes.HasPrefix(writer.pending, []byte(runprogress.MarkerPrefix)) {
			return errors.New("step progress marker exceeds its byte bound")
		}
		if err := writeProgressBytes(writer.destination, writer.pending); err != nil {
			return err
		}
		writer.pending = writer.pending[:0]
		writer.passthrough = true
		return nil
	}
	return nil
}

func (writer *stepProgressLogWriter) processLine(line []byte) error {
	event, found, err := runprogress.ParseMarker(line)
	if err != nil {
		return fmt.Errorf("parse runner step progress: %w", err)
	}
	if !found {
		return writeProgressBytes(writer.destination, line)
	}
	if writer.persist == nil {
		return errors.New("step progress persistence is unavailable")
	}
	if err := writer.persist(event); err != nil {
		return fmt.Errorf("persist runner step progress: %w", err)
	}
	return nil
}

func writeProgressBytes(destination io.Writer, body []byte) error {
	for len(body) > 0 {
		written, err := destination.Write(body)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		body = body[written:]
	}
	return nil
}
