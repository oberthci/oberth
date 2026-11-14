package runlog

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// ContainerLog binds one retained copy to a native Pod and container. Its
// bytes occupy an immutable span in the ordinary run log, without markers.
type ContainerLog struct {
	// ParentNodeID is a verified native Container boundary supplied by tracking.
	// It authorizes only a provisional Pod-to-Container attribution refinement.
	ParentNodeID  string    `json:"-"`
	NodeID        string    `json:"node_id"`
	PodUID        string    `json:"pod_uid"`
	PodName       string    `json:"pod_name"`
	Namespace     string    `json:"namespace,omitempty"`
	ContainerName string    `json:"container_name"`
	Start         int64     `json:"start"`
	End           int64     `json:"end"`
	Reason        string    `json:"reason,omitempty"`
	Pending       bool      `json:"pending,omitempty"`
	Captured      bool      `json:"captured,omitempty"`
	Reader        io.Reader `json:"-"`
	Stage         string    `json:"burn,omitempty"`
	Step          string    `json:"step,omitempty"`
	Main          bool      `json:"main,omitempty"`
}

type ContainerLogStore interface {
	ContainerLogs(context.Context, string) ([]ContainerLog, error)
	TrackContainerLogs(string, []ContainerLog) error
	ReserveContainerLog(string, ContainerLog) (bool, error)
	ReleaseContainerLog(string, ContainerLog) error
	StoreContainerLog(string, ContainerLog, int64) error
}

func (store *Store) openRunLog(runID string, flags int) (*os.File, error) {
	if !safeID(runID) {
		return nil, errors.New("invalid run ID")
	}
	root, err := os.OpenRoot(store.directory)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	file, err := root.OpenFile(runID+".log", flags, 0o640)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, errors.New("run log must be a regular file")
	}
	return file, nil
}

// AppendRunLog serializes ordinary run output with complete container spans.
func (store *Store) AppendRunLog(runID string, body []byte) (int, error) {
	store.containerMu.Lock()
	defer store.containerMu.Unlock()
	file, err := store.openRunLog(runID, os.O_CREATE|os.O_APPEND|os.O_WRONLY)
	if err != nil {
		return 0, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return 0, err
	}
	available := max(int64(0), store.limitBytes-info.Size())
	if int64(len(body)) <= available {
		return file.Write(body)
	}
	const reason = "\noberth: log incomplete: run log limit reached\n"
	kept := min(int64(len(body)), max(int64(0), available-int64(len(reason))))
	if _, err := file.Write(body[:kept]); err != nil {
		return 0, err
	}
	if int64(len(reason)) <= available-kept {
		if _, err := io.WriteString(file, reason); err != nil {
			return 0, err
		}
	}
	return len(body), nil
}

func containerLogPosition(index Index, entry ContainerLog) int {
	for i, saved := range index.Containers {
		if saved.PodUID == entry.PodUID && saved.ContainerName == entry.ContainerName && (entry.PodUID != "" || saved.NodeID == entry.NodeID) {
			return i
		}
	}
	return -1
}

// TrackContainerLogs retains native bindings while Pods still exist, so a later
// deletion can be explained without guessing a Pod name or fetching logs early.
func (store *Store) TrackContainerLogs(runID string, entries []ContainerLog) error {
	store.containerMu.Lock()
	defer store.containerMu.Unlock()
	index, err := store.loadIndexContext(context.Background(), runID)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	changed := false
	for _, entry := range entries {
		if entry.NodeID == "" || entry.PodUID == "" || entry.PodName == "" || entry.ContainerName == "" {
			return errors.New("invalid native container binding")
		}
		if position := containerLogPosition(index, entry); position >= 0 {
			saved := index.Containers[position]
			if saved.Namespace != entry.Namespace || saved.PodName != entry.PodName {
				return errors.New("native container physical binding changed")
			}
			if saved.NodeID != entry.NodeID && saved.NodeID != entry.ParentNodeID {
				continue
			}
			if saved.NodeID != entry.NodeID || saved.Stage != entry.Stage || saved.Step != entry.Step || saved.Main != entry.Main {
				index.Containers[position] = saved.withAttribution(entry)
				changed = true
			}
			continue
		}
		entry.Start, entry.End, entry.Reader, entry.Pending, entry.Captured = index.Size, index.Size, nil, false, false
		index.Containers = append(index.Containers, entry)
		changed = true
	}
	if !changed {
		return nil
	}
	index.RunID, index.Format = runID, "containers"
	return store.writeIndex(runID, index)
}

// ReserveContainerLog records the once-only read before network I/O. An owner
// interrupted during capture leaves an explicit incomplete receipt, not cursors
// or a replay stream. Existing copies and interrupted reads are not reopened.
func (store *Store) ReserveContainerLog(runID string, entry ContainerLog) (bool, error) {
	if entry.NodeID == "" || entry.PodUID == "" || entry.ContainerName == "" {
		return false, errors.New("invalid container log identity")
	}
	store.containerMu.Lock()
	defer store.containerMu.Unlock()
	index, err := store.loadIndexContext(context.Background(), runID)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	position := containerLogPosition(index, entry)
	if position >= 0 && (index.Containers[position].Captured || index.Containers[position].Pending) {
		return false, nil
	}
	if position >= 0 {
		entry = entry.withAttribution(index.Containers[position])
	}
	file, err := store.openRunLog(runID, os.O_CREATE|os.O_APPEND|os.O_WRONLY)
	if err != nil {
		return false, err
	}
	info, statErr := file.Stat()
	_ = file.Close()
	if statErr != nil {
		return false, statErr
	}
	entry.Start, entry.End, entry.Reader = info.Size(), info.Size(), nil
	entry.Pending = true
	entry.Reason = "oberth: log incomplete: container copy has not completed"
	index.RunID, index.Format, index.Size = runID, "containers", info.Size()
	if position >= 0 {
		index.Containers[position] = entry
	} else {
		index.Containers = append(index.Containers, entry)
	}
	return true, store.writeIndex(runID, index)
}

// ReleaseContainerLog releases only a reservation whose source was never opened.
// The serial observer owns it; attempted copies keep their immutable receipt.
func (store *Store) ReleaseContainerLog(runID string, entry ContainerLog) error {
	store.containerMu.Lock()
	defer store.containerMu.Unlock()
	index, err := store.loadIndexContext(context.Background(), runID)
	if err != nil {
		return err
	}
	position := containerLogPosition(index, entry)
	if position < 0 {
		return nil
	}
	saved := index.Containers[position]
	if saved.Namespace != entry.Namespace || saved.PodName != entry.PodName {
		return errors.New("native container physical binding changed")
	}
	if !saved.Pending || saved.Captured || saved.Start != saved.End {
		return nil
	}
	saved.Pending, saved.Reason = false, ""
	index.Containers[position] = saved
	return store.writeIndex(runID, index)
}

// StoreContainerLog streams network bytes to an unlinked temporary file without
// holding append/index locks. Only the bounded disk-to-disk commit is serialized;
// a slow kubelet cannot block progress writes, identity delivery or log readers.
func (store *Store) StoreContainerLog(runID string, entry ContainerLog, limit int64) error {
	if !safeID(runID) || entry.NodeID == "" || limit <= 0 || (entry.Reason == "" && (entry.PodUID == "" || entry.ContainerName == "")) {
		return errors.New("invalid retained container log identity or limit")
	}
	spool, err := os.CreateTemp(store.directory, ".container-*")
	if err != nil {
		return err
	}
	defer func() { _ = spool.Close() }()
	if err := os.Remove(spool.Name()); err != nil {
		return err
	}
	var size int64
	if entry.Reader != nil {
		size, err = io.CopyBuffer(spool, io.LimitReader(entry.Reader, limit), make([]byte, 4096))
		if err != nil {
			entry.Reason = "oberth: log incomplete: container log read was interrupted"
		}
		if size == limit && err == nil {
			var extra [1]byte
			n, readErr := entry.Reader.Read(extra[:])
			if n > 0 {
				entry.Reason = "oberth: log incomplete: run log limit reached"
			} else if readErr != nil && readErr != io.EOF {
				entry.Reason = "oberth: log incomplete: container log read was interrupted"
			}
		}
	}
	entry.Reader = nil
	return store.commitContainerLog(runID, entry, limit, spool, size)
}

func (store *Store) commitContainerLog(runID string, entry ContainerLog, limit int64, spool *os.File, size int64) error {
	store.containerMu.Lock()
	defer store.containerMu.Unlock()
	index, err := store.loadIndexContext(context.Background(), runID)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	position := containerLogPosition(index, entry)
	if position >= 0 && index.Containers[position].Captured {
		return nil
	}
	if position >= 0 {
		entry = entry.withAttribution(index.Containers[position])
	}
	file, err := store.openRunLog(runID, os.O_CREATE|os.O_APPEND|os.O_WRONLY)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	available := max(int64(0), limit-info.Size())
	if size > available {
		entry.Reason = "oberth: log incomplete: run log limit reached"
	}
	reason := ""
	if entry.Reason != "" {
		reason = "\n" + entry.Reason + "\n"
	}
	bodySize := min(size, max(int64(0), available-int64(len(reason))))
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		return err
	}
	n, err := io.CopyBuffer(file, io.LimitReader(spool, bodySize), make([]byte, 4096))
	if err != nil {
		return err
	}
	if int64(len(reason)) <= available-n {
		written, err := io.Copy(file, strings.NewReader(reason))
		n += written
		if err != nil {
			return err
		}
	}
	if err := file.Sync(); err != nil {
		return err
	}
	entry.Start, entry.End, entry.Pending, entry.Captured = info.Size(), info.Size()+n, false, true
	index.RunID, index.Format, index.Size = runID, "containers", entry.End
	if position >= 0 {
		index.Containers[position] = entry
	} else {
		index.Containers = append(index.Containers, entry)
	}
	return store.writeIndex(runID, index)
}

func (store *Store) ContainerLogs(ctx context.Context, runID string) ([]ContainerLog, error) {
	store.containerMu.Lock()
	defer store.containerMu.Unlock()
	index, err := store.loadIndexContext(ctx, runID)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return index.Containers, err
}

func (store *Store) ReadContainerLog(ctx context.Context, runID, nodeID, container string, filter Filter) ([]byte, Meta, ContainerLog, error) {
	entries, err := store.ContainerLogs(ctx, runID)
	if err != nil {
		return nil, Meta{}, ContainerLog{}, err
	}
	for _, entry := range entries {
		if entry.NodeID != nodeID || entry.ContainerName != container {
			continue
		}
		if !entry.Captured && !entry.Pending {
			return nil, Meta{}, entry, os.ErrNotExist
		}
		if entry.Start < 0 || entry.End < entry.Start {
			return nil, Meta{}, entry, errors.New("invalid container log range")
		}
		if entry.Start == entry.End {
			var body []byte
			if entry.Reason != "" {
				body = []byte(entry.Reason + "\n")
			}
			value, meta, err := FilterBytes(body, filter.WithContext(ctx))
			return value, meta, entry, err
		}
		file, err := store.openRunLog(runID, os.O_RDONLY)
		if err != nil {
			return nil, Meta{}, entry, err
		}
		defer func() { _ = file.Close() }()
		info, err := file.Stat()
		if err != nil {
			return nil, Meta{}, entry, err
		}
		if entry.End > info.Size() {
			return nil, Meta{}, entry, errors.New("container log is incomplete in retained storage")
		}
		body, meta, err := filterRanges(file, []Range{{Start: entry.Start, End: entry.End}}, entry.End-entry.Start, filter.WithContext(ctx))
		return body, meta, entry, err
	}
	return nil, Meta{}, ContainerLog{}, fmt.Errorf("container log %s/%s: %w", nodeID, container, os.ErrNotExist)
}

// ReadRunLog displays historical marker-bearing files as ordinary text.
func (store *Store) ReadRunLog(ctx context.Context, runID string, filter Filter) ([]byte, Meta, error) {
	file, err := store.openRunLog(runID, os.O_RDONLY)
	if err != nil {
		return nil, Meta{}, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, Meta{}, err
	}
	return filterRanges(file, []Range{{Start: 0, End: info.Size()}}, info.Size(), filter.WithContext(ctx))
}

func (store *Store) OpenStoredContainerLog(ctx context.Context, runID, node, container string) (io.ReadCloser, error) {
	entries, err := store.ContainerLogs(ctx, runID)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.NodeID != node || entry.ContainerName != container {
			continue
		}
		if !entry.Captured && !entry.Pending {
			return nil, os.ErrNotExist
		}
		if entry.Start == entry.End {
			if entry.Reason != "" {
				return io.NopCloser(bytes.NewBufferString(entry.Reason + "\n")), nil
			}
			return io.NopCloser(bytes.NewReader(nil)), nil
		}
		file, err := store.openRunLog(runID, os.O_RDONLY)
		if err != nil {
			return nil, err
		}
		info, err := file.Stat()
		if err != nil || entry.Start < 0 || entry.End < entry.Start || entry.End > info.Size() {
			_ = file.Close()
			return nil, errors.New("stored container log range is unavailable")
		}
		return &storedContainerLog{Reader: io.NewSectionReader(file, entry.Start, entry.End-entry.Start), file: file}, nil
	}
	return nil, os.ErrNotExist
}

type storedContainerLog struct {
	io.Reader
	file *os.File
}

func (stream *storedContainerLog) Close() error { return stream.file.Close() }

func (store *Store) readNativeStepLog(ctx context.Context, runID, stage, step string, index Index, filter Filter) ([]byte, Meta, error) {
	var selected []Range
	var total int64
	var reason string
	for _, entry := range index.Containers {
		if (step != "" && entry.NodeID != step) || (step == "" && entry.Stage != stage) || (!entry.Main && entry.ContainerName != "main" && entry.ContainerName != "") {
			continue
		}
		if !entry.Captured && !entry.Pending {
			continue
		}
		selected = append(selected, Range{Start: entry.Start, End: entry.End})
		total += entry.End - entry.Start
		if entry.Reason != "" {
			reason = entry.Reason
		}
	}
	if len(selected) == 0 {
		return nil, Meta{}, fmt.Errorf("native log slice %s/%s: %w", stage, step, os.ErrNotExist)
	}
	if total == 0 {
		return FilterBytes([]byte(reasonLine(reason)), filter.WithContext(ctx))
	}
	file, err := store.openRunLog(runID, os.O_RDONLY)
	if err != nil {
		return nil, Meta{}, err
	}
	defer func() { _ = file.Close() }()
	return filterRanges(file, selected, total, filter.WithContext(ctx))
}

func reasonLine(reason string) string {
	if reason == "" {
		return ""
	}
	return reason + "\n"
}

// Attribution may advance while a physical copy is reserved or being read.
// Its receipt state, reason and immutable byte span belong to that same copy.
func (entry ContainerLog) withAttribution(source ContainerLog) ContainerLog {
	entry.NodeID, entry.Stage, entry.Step, entry.Main = source.NodeID, source.Stage, source.Step, source.Main
	return entry
}
