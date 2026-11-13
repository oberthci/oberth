package runlog

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
)

// ContainerLog identifies a single retained copy. Bytes stay in the run's
// ordinary .log file; only attribution and missing/incomplete reasons live
// in its existing .index.json. There are no in-band control records.
type ContainerLog struct {
	NodeID        string `json:"node_id"`
	PodUID        string `json:"pod_uid"`
	PodName       string `json:"pod_name"`
	ContainerName string `json:"container_name"`
	Start         int64  `json:"start"`
	End           int64  `json:"end"`
	Reason        string `json:"reason,omitempty"`
	Body          []byte `json:"-"`
}

type ContainerLogStore interface {
	ContainerLogStored(runID, podUID, container string) (bool, error)
	StoreContainerLog(runID string, entry ContainerLog, limit int64) error
}

func (store *Store) ContainerLogStored(runID, podUID, container string) (bool, error) {
	store.containerMu.Lock()
	defer store.containerMu.Unlock()
	index, err := store.loadIndexContext(context.Background(), runID)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, entry := range index.Containers {
		if entry.PodUID == podUID && entry.ContainerName == container {
			return true, nil
		}
	}
	return false, nil
}

func (store *Store) StoreContainerLog(runID string, entry ContainerLog, limit int64) error {
	if entry.NodeID == "" || entry.PodUID == "" || entry.ContainerName == "" || limit <= 0 {
		return errors.New("invalid retained container log identity or limit")
	}
	store.containerMu.Lock()
	defer store.containerMu.Unlock()
	path, err := store.logPath(runID)
	if err != nil {
		return err
	}
	index, err := store.loadIndexContext(context.Background(), runID)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, previous := range index.Containers {
		if previous.PodUID == entry.PodUID && previous.ContainerName == entry.ContainerName {
			return nil
		}
	}
	// The run ID has been validated and confined to the log root.
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640) //nolint:gosec
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("retained run log must be a regular file")
	}
	remaining := max(int64(0), limit-info.Size())
	body := bytes.Clone(entry.Body)
	if int64(len(body)) > remaining {
		entry.Reason = "oberth: log incomplete: retained run log limit reached"
	}
	if entry.Reason != "" {
		reason := []byte(entry.Reason + "\n")
		if int64(len(reason)) <= remaining {
			body = body[:min(int64(len(body)), remaining-int64(len(reason)))]
			if len(body) > 0 && body[len(body)-1] != '\n' {
				// Reserve the separator without exceeding the existing budget.
				if int64(len(body)+len(reason)+1) > remaining {
					body = body[:len(body)-1]
				}
				body = append(body, '\n')
			}
			body = append(body, reason...)
		} else {
			// Keep the full reason in the index even when no file bytes remain.
			body = nil
		}
	} else if int64(len(body)) > remaining {
		body = body[:remaining]
	}
	n, err := file.Write(body)
	if err != nil {
		return err
	}
	if n != len(body) {
		return io.ErrShortWrite
	}
	end, err := file.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	entry.Start, entry.End, entry.Body = end-int64(n), end, nil
	index.RunID, index.Format, index.Size = runID, "containers", end
	index.Containers = append(index.Containers, entry)
	return store.writeIndex(runID, index)
}

func (store *Store) ReadContainerLog(ctx context.Context, runID, nodeID, container string, filter Filter) ([]byte, Meta, ContainerLog, error) {
	store.containerMu.Lock()
	index, err := store.loadIndexContext(ctx, runID)
	store.containerMu.Unlock()
	if err != nil {
		return nil, Meta{}, ContainerLog{}, err
	}
	for _, entry := range index.Containers {
		if entry.NodeID != nodeID || entry.ContainerName != container {
			continue
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
		path, err := store.logPath(runID)
		if err != nil {
			return nil, Meta{}, entry, err
		}
		file, err := os.Open(path) //nolint:gosec // Validated run ID confines this to the log root.
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

// ReadRunLog keeps historical marker-bearing files readable as plain text.
func (store *Store) ReadRunLog(ctx context.Context, runID string, filter Filter) ([]byte, Meta, error) {
	path, err := store.logPath(runID)
	if err != nil {
		return nil, Meta{}, err
	}
	file, err := os.Open(path) //nolint:gosec // Validated run ID confines this to the log root.
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
