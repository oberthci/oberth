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

// PodLogContinuityLostMarker exposes degraded delivery without turning an optional
// journal into execution authority or a scheduler/component failure.
const PodLogContinuityLostMarker = "oberth: log continuity gap: durable pod journal unavailable; continuing without restart cursors"

var ErrPodLogProjection = errors.New("pod log projection write failed")

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
	log       *os.File
	journal   *os.File
	cursors   map[string]PodLogCursor
	limit     int64
	runLimit  int64
	projected int64
}

func OpenPodLogJournal(file *os.File, runLimit int64) (*PodLogJournal, error) {
	if runLimit <= 0 || runLimit > 1<<40 {
		return nil, errors.New("invalid pod journal budget")
	}
	// A prior degradation is recorded in the ordinary retained log. Never
	// trust a stale journal again if removal failed (for example permissions).
	if lost, err := podLogContinuityLost(file, runLimit); err != nil {
		return nil, err
	} else if lost {
		return nil, errors.New("pod log continuity was previously lost")
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
		j.projected = projectionEnd
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
	if n == int64(len(frame.Data)) && bytes.Equal(actual, frame.Data) {
		return nil
	}
	// A valid checksummed WAL contains actual received bytes, even if an
	// unsynced projection was torn/zeroed. Repair only its recorded range;
	// never fill a legacy gap or overwrite an unjournaled trailing range.
	repair, err := os.OpenFile(j.log.Name(), os.O_RDWR, 0) //nolint:gosec // Same validated, run-owned log; separate descriptor avoids O_APPEND.
	if err != nil {
		return err
	}
	defer func() { _ = repair.Close() }()
	_, err = repair.WriteAt(frame.Data, frame.Offset)
	return err
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
	info, err := j.log.Stat()
	if err != nil {
		return err
	}
	if info.Size()+int64(len(body)) > j.runLimit || info.Size() < j.projected {
		return errors.New("pod log projection byte budget or prefix changed")
	}
	// Plain progress/delivery/seed writes between commits belong to the
	// recoverable prefix too. Snapshot them in bounded frames, retaining the
	// OLD cursor until the body frame. A torn prefix batch cannot advance a
	// source cursor past output that was never projected.
	prefixCursor, ok := j.cursors[cursor.Key]
	if !ok {
		prefixCursor = PodLogCursor{Key: cursor.Key, WorkflowUID: cursor.WorkflowUID, Node: cursor.Node, Pod: cursor.Pod, Container: cursor.Container}
	}
	for offset := j.projected; offset < info.Size(); {
		length := min(int64(1<<20), info.Size()-offset)
		prefix := make([]byte, length)
		if _, err := j.log.ReadAt(prefix, offset); err != nil {
			return err
		}
		if err := j.appendFrame(podLogFrame{Version: 1, Offset: offset, Cursor: prefixCursor, Data: prefix}); err != nil {
			return err
		}
		offset += length
	}
	if err := j.appendFrame(podLogFrame{Version: 1, Offset: info.Size(), Cursor: cursor, Data: body}); err != nil {
		return err
	}
	// One durability barrier covers prefix, bytes and checkpoint. The ordinary
	// log is a projection; terminal finalization syncs it before WAL removal.
	if err := j.journal.Sync(); err != nil {
		return err
	}
	if len(body) > 0 {
		n, err := write(body)
		if err != nil {
			return errors.Join(ErrPodLogProjection, err)
		}
		if n != len(body) {
			return errors.Join(ErrPodLogProjection, io.ErrShortWrite)
		}
	}
	j.projected = info.Size() + int64(len(body))
	j.cursors[cursor.Key] = cursor
	return nil
}

func (j *PodLogJournal) appendFrame(frame podLogFrame) error {
	var err error
	frame.SHA256, err = frameDigest(frame)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	info, err := j.journal.Stat()
	if err != nil {
		return err
	}
	if info.Size()+int64(len(encoded)) > j.limit {
		return errors.New("pod log journal byte budget reached")
	}
	_, err = j.journal.Write(encoded)
	return err
}

func (j *PodLogJournal) Cursors() map[string]PodLogCursor {
	result := make(map[string]PodLogCursor, len(j.cursors))
	for key, value := range j.cursors {
		result[key] = value
	}
	return result
}
func (j *PodLogJournal) Close() error { return j.journal.Close() }

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

// PrunePodLogJournal removes only the journal attached to an already-owned run
// log path. Directories/symlinks are not recursively removed or followed.
func PrunePodLogJournal(logPath string) error {
	name := logPath + ".pods.jsonl"
	info, err := os.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("refuse nonregular pod journal")
	}
	if err := os.Remove(name); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(name))
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}

func podLogContinuityLost(file *os.File, limit int64) (bool, error) {
	// ReadAt does not disturb append positioning. A source line always carries
	// [burn/step], so only an exact unattributed server record is a marker.
	reader := bufio.NewReaderSize(io.NewSectionReader(file, 0, limit), 64<<10)
	marker := []byte(PodLogContinuityLostMarker + "\n")
	matched, eligible := 0, true
	for {
		fragment, err := reader.ReadSlice('\n')
		if eligible {
			if matched+len(fragment) > len(marker) || !bytes.Equal(fragment, marker[matched:matched+len(fragment)]) {
				eligible = false
			} else {
				matched += len(fragment)
			}
		}
		if err == nil {
			if eligible && matched == len(marker) {
				return true, nil
			}
			matched, eligible = 0, true
			continue
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		return false, err
	}
}
