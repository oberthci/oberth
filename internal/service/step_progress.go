package service

import (
	"errors"
	"io"
	"os"

	"github.com/oberthci/oberth/internal/runlog"
)

// stepProgressLogWriter projects plain run-log bytes and preserves Pod log durability.
type stepProgressLogWriter struct {
	journal          *runlog.PodLogJournal
	journalErr       error
	durabilityLost   bool
	onContinuityLost func()
	logFile          *os.File
	destination      io.Writer
	closed           bool
}

func newStepProgressLogWriter(destination io.Writer) *stepProgressLogWriter {
	return &stepProgressLogWriter{destination: destination}
}

func (writer *stepProgressLogWriter) Write(body []byte) (int, error) {
	if writer.journalErr != nil {
		return 0, writer.journalErr
	}
	if writer.closed {
		return 0, errors.New("run log writer is closed")
	}
	return writer.destination.Write(body)
}

func (writer *stepProgressLogWriter) Close() error {
	writer.closed = true
	return writer.journalErr
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
// The WAL restores the full received frame without appending it twice.
func (writer *stepProgressLogWriter) recoverPodProjection(limit int64) {
	if writer.journalErr == nil || writer.journal == nil || writer.logFile == nil {
		return
	}
	_ = writer.journal.Close()
	writer.journal = nil
	recovered, err := runlog.OpenPodLogJournal(writer.logFile, limit)
	writer.journalErr = nil
	if err != nil {
		_, _ = writer.logFile.Seek(0, io.SeekEnd)
		writer.losePodLogDurability()
		return
	}
	writer.journal = recovered
}
