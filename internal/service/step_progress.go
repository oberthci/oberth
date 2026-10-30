package service

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/runprogress"
)

// stepProgressLogWriter removes runner control records from the retained user
// log and journals them as they arrive. It buffers at most one bounded marker;
// ordinary long lines stream directly to the retained-log limiter.
type stepProgressLogWriter struct {
	journal          *runlog.PodLogJournal
	journalErr       error
	durabilityLost   bool
	onContinuityLost func()
	logFile          *os.File
	destination      io.Writer
	persist          func(runprogress.Event) error
	pending          []byte
	passthrough      bool
	closed           bool
}

func newStepProgressLogWriter(destination io.Writer, persist func(runprogress.Event) error) *stepProgressLogWriter {
	return &stepProgressLogWriter{destination: destination, persist: persist}
}

func (writer *stepProgressLogWriter) Write(body []byte) (int, error) {
	if writer.journalErr != nil {
		return 0, writer.journalErr
	}
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
	if writer.journalErr != nil {
		writer.closed = true
		return writer.journalErr
	}
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

func (writer *stepProgressLogWriter) CommitPodLog(cursor runlog.PodLogCursor, body []byte) error {
	if writer.journal == nil {
		_, err := writer.Write(body)
		return err
	}
	err := writer.journal.Commit(cursor, body, writer.Write)
	if err == nil {
		return nil
	}
	if errors.Is(err, runlog.ErrPodLogProjection) {
		// A partial projection still has its valid WAL and must not be appended
		// again. Preserve it for recovery and stop only this log writer.
		writer.journalErr = err
		return err
	}
	// No projection callback has run on a journal failure: retain the actual
	// body once, abandon cursor authority, and keep observing the Workflow.
	_ = writer.journal.Close()
	writer.journal = nil
	_, writeErr := writer.Write(body)
	writer.losePodLogDurability()
	return writeErr
}

func (writer *stepProgressLogWriter) PodLogContinuityLost() bool { return writer.durabilityLost }
func (writer *stepProgressLogWriter) losePodLogDurability() {
	if writer.durabilityLost {
		return
	}
	writer.durabilityLost = true
	if writer.onContinuityLost != nil {
		writer.onContinuityLost()
	}
	_, _ = writer.Write([]byte("\n" + runlog.PodLogContinuityLostMarker + "\n"))
	if writer.logFile != nil {
		// Once per degradation, not once per burst. The marker prevents reuse of
		// a stale journal even when filesystem permissions prevent its removal.
		if writer.logFile.Sync() == nil {
			_ = runlog.PrunePodLogJournal(writer.logFile.Name())
		}
	}
}

func (writer *stepProgressLogWriter) PodLogCursors() map[string]runlog.PodLogCursor {
	if writer.journal == nil {
		return nil
	}
	return writer.journal.Cursors()
}

// Restore a valid WAL after a partial projection before final sync/index/prune.
// Dropping the parser's pending fragment is safe only after restoring the full
// received frame; retrying that fragment through Write would duplicate bytes.
func (writer *stepProgressLogWriter) recoverPodProjection(limit int64) {
	if writer.journalErr == nil || writer.journal == nil || writer.logFile == nil {
		return
	}
	_ = writer.journal.Close()
	writer.journal = nil
	recovered, err := runlog.OpenPodLogJournal(writer.logFile, limit)
	writer.pending = nil
	writer.passthrough = false
	writer.journalErr = nil
	if err != nil {
		_, _ = writer.logFile.Seek(0, io.SeekEnd)
		writer.losePodLogDurability()
		return
	}
	writer.journal = recovered
}
