package redact

import (
	"bytes"
	"errors"
	"io"
	"slices"
)

var ErrClosed = errors.New("redacting writer is closed")

type Writer struct {
	destination io.Writer
	secrets     [][]byte
	buffer      []byte
	closed      bool
}

func NewWriter(destination io.Writer, values [][]byte) *Writer {
	secrets := make([][]byte, 0, len(values))
	seen := map[string]bool{}
	addSecret := func(value []byte) {
		if len(value) == 0 || seen[string(value)] {
			return
		}
		copyValue := slices.Clone(value)
		secrets = append(secrets, copyValue)
		seen[string(copyValue)] = true
	}
	for _, value := range values {
		addSecret(value)
		addSecret(bytes.TrimRight(value, "\r\n"))
		normalized := bytes.ReplaceAll(value, []byte("\r\n"), []byte("\n"))
		for _, line := range bytes.Split(normalized, []byte("\n")) {
			addSecret(line)
		}
	}
	slices.SortFunc(secrets, func(left, right []byte) int { return len(right) - len(left) })
	return &Writer{destination: destination, secrets: secrets}
}

func (writer *Writer) Write(body []byte) (int, error) {
	if writer.closed {
		return 0, ErrClosed
	}
	want := len(body)
	writer.buffer = append(writer.buffer, body...)
	if err := writer.drain(false); err != nil {
		return 0, err
	}
	return want, nil
}

func (writer *Writer) Flush() error {
	if writer.closed {
		return ErrClosed
	}
	return writer.drain(true)
}

func (writer *Writer) Close() error {
	if writer.closed {
		return nil
	}
	err := writer.Flush()
	writer.closed = true
	return err
}

func (writer *Writer) drain(final bool) error {
	for len(writer.buffer) > 0 {
		matchIndex, matchLength := writer.firstMatch()
		if matchIndex >= 0 {
			if err := writeAll(writer.destination, writer.buffer[:matchIndex]); err != nil {
				return err
			}
			if err := writeAll(writer.destination, []byte("***")); err != nil {
				return err
			}
			writer.buffer = writer.buffer[matchIndex+matchLength:]
			continue
		}
		retain := 0
		if !final {
			retain = writer.partialSuffixLength()
		}
		emit := len(writer.buffer) - retain
		if err := writeAll(writer.destination, writer.buffer[:emit]); err != nil {
			return err
		}
		writer.buffer = slices.Clone(writer.buffer[emit:])
		return nil
	}
	return nil
}

func (writer *Writer) firstMatch() (int, int) {
	bestIndex, bestLength := -1, 0
	for _, secret := range writer.secrets {
		index := bytes.Index(writer.buffer, secret)
		if index < 0 {
			continue
		}
		if bestIndex < 0 || index < bestIndex || (index == bestIndex && len(secret) > bestLength) {
			bestIndex, bestLength = index, len(secret)
		}
	}
	return bestIndex, bestLength
}

func (writer *Writer) partialSuffixLength() int {
	best := 0
	for _, secret := range writer.secrets {
		limit := min(len(writer.buffer), len(secret)-1)
		for length := limit; length > best; length-- {
			if bytes.Equal(writer.buffer[len(writer.buffer)-length:], secret[:length]) {
				best = length
				break
			}
		}
	}
	return best
}

func writeAll(destination io.Writer, body []byte) error {
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
