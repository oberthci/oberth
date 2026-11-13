package runlog

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
)

// ContainerLog binds one retained copy to a native Pod and container. Its
// bytes occupy an immutable span in the ordinary run log, without markers.
type ContainerLog struct {
	NodeID        string `json:"node_id"`
	PodUID        string `json:"pod_uid"`
	PodName       string `json:"pod_name"`
	Namespace     string `json:"namespace,omitempty"`
	ContainerName string `json:"container_name"`
	Start         int64  `json:"start"`
	End           int64  `json:"end"`
	Reason        string `json:"reason,omitempty"`
	Pending       bool   `json:"pending,omitempty"`
	Captured      bool   `json:"captured,omitempty"`
	Body          []byte `json:"-"`
}

type ContainerLogStore interface {
	ContainerLogs(context.Context, string) ([]ContainerLog, error)
	TrackContainerLogs(string, []ContainerLog) error
	ReserveContainerLog(string, ContainerLog) (bool, error)
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
	return file.Write(body)
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
		if containerLogPosition(index, entry) >= 0 {
			continue
		}
		entry.Start, entry.End, entry.Body, entry.Pending, entry.Captured = index.Size, index.Size, nil, false, false
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
	file, err := store.openRunLog(runID, os.O_CREATE|os.O_APPEND|os.O_WRONLY)
	if err != nil {
		return false, err
	}
	info, statErr := file.Stat()
	_ = file.Close()
	if statErr != nil {
		return false, statErr
	}
	entry.Start, entry.End, entry.Body = info.Size(), info.Size(), nil
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

func retainedContainerBody(entry *ContainerLog, limit int64) []byte {
	body := bytes.Clone(entry.Body)
	if int64(len(body)) > limit {
		entry.Reason = "oberth: log incomplete: container log limit reached"
	}
	if entry.Reason == "" {
		return body[:min(int64(len(body)), limit)]
	}
	reason := []byte(entry.Reason + "\n")
	if int64(len(reason)) > limit {
		return nil
	}
	body = body[:min(int64(len(body)), limit-int64(len(reason)))]
	if len(body) > 0 && body[len(body)-1] != '\n' {
		if int64(len(body)+len(reason)+1) > limit {
			body = body[:len(body)-1]
		}
		body = append(body, '\n')
	}
	return append(body, reason...)
}

func (store *Store) StoreContainerLog(runID string, entry ContainerLog, limit int64) error {
	if entry.NodeID == "" || limit <= 0 || (entry.Reason == "" && (entry.PodUID == "" || entry.ContainerName == "")) {
		return errors.New("invalid retained container log identity or limit")
	}
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
	file, err := store.openRunLog(runID, os.O_CREATE|os.O_APPEND|os.O_WRONLY)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	body := retainedContainerBody(&entry, limit)
	n, err := file.Write(body)
	if err != nil {
		return err
	}
	if n != len(body) {
		return io.ErrShortWrite
	}
	if err := file.Sync(); err != nil {
		return err
	}
	entry.Start, entry.End, entry.Body, entry.Pending, entry.Captured = info.Size(), info.Size()+int64(n), nil, false, true
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
