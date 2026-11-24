package runlog

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"os"
)

func (s *Store) load(id string) (index, error) {
	empty := index{RunID: id, Containers: []ContainerLog{}}
	name := id + ".index.json"
	info, err := s.root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return empty, nil
	}
	if err != nil {
		return empty, storage(cannotInspectLogIndexReason, err)
	}
	if !info.Mode().IsRegular() {
		return empty, storage(invalidLogIndexFileOrSizeReason, nil)
	}
	file, err := s.root.Open(name)
	if err != nil {
		return empty, storage(cannotOpenLogIndexReason, err)
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, maxIndexBytes+1))
	if err != nil || len(data) > maxIndexBytes {
		return empty, storage(cannotReadLogIndexReason, err)
	}
	return decodeIndex(data, id)
}
func decodeIndex(data []byte, id string) (index, error) {
	empty := index{}
	var saved index
	if err := json.Unmarshal(data, &saved); err != nil || saved.RunID != id || saved.Size < 0 {
		return empty, storage(invalidRetainedLogIndexReason, err)
	}
	for _, entry := range saved.Containers {
		if entry.Start < 0 || entry.End < entry.Start || entry.End > saved.Size {
			return empty, storage(invalidRetainedContainerSpanReason, nil)
		}
	}
	return saved, nil
}
func (s *Store) save(id string, saved index) error {
	data, err := json.Marshal(saved)
	if err != nil || len(data) > maxIndexBytes {
		return storage(logIndexExceedsIts8MiBLimitReason, err)
	}
	name := ".index-" + rand.Text()
	file, err := s.root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return storage(cannotStageLogIndexReason, err)
	}
	defer func() { _ = file.Close(); _ = s.root.Remove(name) }()
	if _, err = file.Write(data); err != nil {
		return storage(cannotWriteLogIndexReason, err)
	}
	if err = file.Sync(); err != nil {
		return storage(cannotSyncLogIndexReason, err)
	}
	if err = file.Close(); err != nil {
		return storage(cannotCloseLogIndexReason, err)
	}
	// A same-directory rename publishes the complete index after body sync.
	if err = s.root.Rename(name, id+".index.json"); err != nil {
		return storage(cannotPublishLogIndexReason, err)
	}
	return s.syncDirectory()
}

func (s *Store) syncDirectory() error {
	directory, err := s.root.Open(".")
	if err != nil {
		return storage(cannotOpenLogDirectoryForSyncReason, err)
	}
	defer func() { _ = directory.Close() }()
	if err = directory.Sync(); err != nil {
		return storage(cannotSyncLogDirectoryReason, err)
	}
	return nil
}
