package runlog

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

// PodLogCursor binds a reconnect boundary to one workflow/node/container.
// Counts and digest preserve repeated identical records at inclusive timestamps.
type PodLogCursor struct {
	Key                 string                `json:"key"`
	WorkflowUID         string                `json:"workflow_uid"`
	Node                string                `json:"node"`
	Pod                 string                `json:"pod"`
	Container           string                `json:"container"`
	Identity            string                `json:"identity"`
	At                  time.Time             `json:"at"`
	Count               int                   `json:"count"`
	Digest              [32]byte              `json:"digest"`
	Bytes               int64                 `json:"bytes"`
	Gap                 bool                  `json:"gap,omitempty"`
	SkipBoundary        bool                  `json:"skip_boundary,omitempty"`
	Termination         *model.StepDiagnostic `json:"termination,omitempty"`
	TerminationReported bool                  `json:"termination_reported,omitempty"`
	Complete            bool                  `json:"complete,omitempty"`
}

type PodLogSink interface {
	CommitPodLog(PodLogCursor, []byte) error
	PodLogCursors() map[string]PodLogCursor
}

type podLogFrame struct {
	Version int          `json:"version"`
	Offset  int64        `json:"offset"`
	Cursor  PodLogCursor `json:"cursor"`
	Data    []byte       `json:"data"`
	SHA256  string       `json:"sha256,omitempty"`
}

// PodLogJournal is a write-ahead record of received bytes plus their cursor.
// The ordinary .log is the user-facing projection. Recovery repairs a crash
// between durable journal append and projection, without fetching or inventing
// bytes. The caller serializes all writes to the run log, including controls.
type PodLogJournal struct {
	log      *os.File
	journal  *os.File
	cursors  map[string]PodLogCursor
	limit    int64
	runLimit int64
}

func OpenPodLogJournal(file *os.File, runLimit int64) (*PodLogJournal, error) {
	if runLimit <= 0 || runLimit > 1<<40 {
		return nil, errors.New("invalid pod journal budget")
	}
	path := file.Name() + ".pods.jsonl"
	journal, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600) //nolint:gosec // Derived from the already validated, server-owned run log path.
	if err != nil {
		return nil, err
	}
	j := &PodLogJournal{log: file, journal: journal, cursors: map[string]PodLogCursor{}, limit: runLimit*2 + (8 << 20), runLimit: runLimit}
	// Persist the directory entry too: a durable cursor must not outlive its
	// newly-created journal after a host crash.
	dir, err := os.Open(filepath.Dir(path))
	if err == nil {
		err = dir.Sync()
		_ = dir.Close()
	}
	if err != nil {
		_ = journal.Close()
		return nil, err
	}
	if err := j.recover(); err != nil {
		_ = journal.Close()
		return nil, err
	}
	return j, nil
}

func frameDigest(frame podLogFrame) (string, error) {
	frame.SHA256 = ""
	body, err := json.Marshal(frame)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:]), nil
}

func (j *PodLogJournal) recover() error {
	info, err := j.journal.Stat()
	if err != nil {
		return err
	}
	if info.Size() > j.limit {
		return errors.New("pod log journal exceeds byte budget")
	}
	reader := bufio.NewReaderSize(j.journal, 64<<10)
	var complete, projectionEnd int64
	for {
		var body []byte
		var err error
		for {
			fragment, readErr := reader.ReadSlice('\n')
			if len(body)+len(fragment) > 2<<20 {
				return errors.New("pod log journal frame exceeds bound")
			}
			body = append(body, fragment...)
			if !errors.Is(readErr, bufio.ErrBufferFull) {
				err = readErr
				break
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				return err
			}
			// A torn final WAL append precedes its projection and is uncommitted.
			if len(body) > 0 {
				if err := j.journal.Truncate(complete); err != nil {
					return err
				}
			}
			break
		}
		var frame podLogFrame
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&frame); err != nil {
			return fmt.Errorf("decode pod log journal: %w", err)
		}
		if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			return errors.New("trailing pod log journal data")
		}
		digest, err := frameDigest(frame)
		if err != nil || frame.Version != 1 || frame.SHA256 != digest || frame.Offset < projectionEnd || frame.Offset+int64(len(frame.Data)) > j.runLimit || !j.validCursor(frame.Cursor) || len(frame.Data) > (1<<20)+8192 {
			return errors.New("invalid pod log journal frame")
		}
		projectionEnd = frame.Offset + int64(len(frame.Data))
		if err := j.repair(frame); err != nil {
			return err
		}
		j.cursors[frame.Cursor.Key] = frame.Cursor
		complete += int64(len(body))
	}
	if _, err := j.journal.Seek(0, io.SeekEnd); err != nil {
		return err
	}
	_, err = j.log.Seek(0, io.SeekEnd)
	return err
}

func (j *PodLogJournal) repair(frame podLogFrame) error {
	info, err := j.log.Stat()
	if err != nil {
		return err
	}
	if info.Size() < frame.Offset {
		return errors.New("pod log projection is missing a committed prefix")
	}
	n := min(int64(len(frame.Data)), info.Size()-frame.Offset)
	actual := make([]byte, n)
	if _, err := j.log.ReadAt(actual, frame.Offset); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if !bytes.Equal(actual, frame.Data[:n]) {
		return errors.New("pod log projection disagrees with journal")
	}
	if n == int64(len(frame.Data)) {
		return nil
	}
	// No later append is valid before a partial projection is repaired.
	if err := j.log.Truncate(frame.Offset); err != nil {
		return err
	}
	if _, err := j.log.Seek(0, io.SeekEnd); err != nil {
		return err
	}
	if _, err := j.log.Write(frame.Data); err != nil {
		return err
	}
	return j.log.Sync()
}

// Commit journals before writing the projection. No cursor can advance beyond
// recoverable received bytes. The write callback preserves existing output and
// progress processing; it must write exactly these already attributed bytes.
func (j *PodLogJournal) Commit(cursor PodLogCursor, body []byte, write func([]byte) (int, error)) error {
	if !j.validCursor(cursor) {
		return errors.New("invalid pod log cursor")
	}
	if len(body) > (1<<20)+8192 {
		return errors.New("pod log frame exceeds bound")
	}
	if err := j.log.Sync(); err != nil {
		return err
	}
	info, err := j.log.Stat()
	if err != nil {
		return err
	}
	if info.Size()+int64(len(body)) > j.runLimit {
		return errors.New("pod log projection byte budget reached")
	}
	frame := podLogFrame{Version: 1, Offset: info.Size(), Cursor: cursor, Data: body}
	frame.SHA256, err = frameDigest(frame)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	journalInfo, err := j.journal.Stat()
	if err != nil {
		return err
	}
	if journalInfo.Size()+int64(len(encoded)) > j.limit {
		return errors.New("pod log journal byte budget reached")
	}
	if _, err := j.journal.Write(encoded); err != nil {
		return err
	}
	if err := j.journal.Sync(); err != nil {
		return err
	}
	if len(body) > 0 {
		n, err := write(body)
		if err != nil {
			return err
		}
		if n != len(body) {
			return io.ErrShortWrite
		}
	}
	if err := j.log.Sync(); err != nil {
		return err
	}
	j.cursors[cursor.Key] = cursor
	return nil
}

func (j *PodLogJournal) Cursors() map[string]PodLogCursor {
	result := make(map[string]PodLogCursor, len(j.cursors))
	for key, value := range j.cursors {
		result[key] = value
	}
	return result
}
func (j *PodLogJournal) Close() error { return errors.Join(j.journal.Sync(), j.journal.Close()) }

func (j *PodLogJournal) validCursor(c PodLogCursor) bool {
	if c.Termination != nil {
		if c.Termination.Validate() != nil {
			return false
		}
		if t := c.Termination.Container; t != nil && (t.WorkflowUID != c.WorkflowUID || t.NodeID != c.Node || t.PodName != c.Pod || t.ContainerName != c.Container) {
			return false
		}
	}
	if c.Key != c.WorkflowUID+"/"+c.Node+"/"+c.Pod || c.Node == "" || c.Pod == "" || c.Container == "" || c.Count < 0 || c.Count > 1<<40 || c.Bytes < 0 || c.Bytes > j.runLimit {
		return false
	}
	for _, s := range []string{c.Key, c.WorkflowUID, c.Node, c.Pod, c.Container, c.Identity} {
		if len(s) > 1024 || strings.ContainsAny(s, "\x00\r\n") {
			return false
		}
	}
	return !c.At.IsZero() || c.Count == 0
}
