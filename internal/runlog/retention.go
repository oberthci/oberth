package runlog

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
)

// UsageByRun counts only regular bodies and current native indexes;
// symlink/nonregular evidence is skipped without following it.
func (s *Store) UsageByRun(ctx context.Context) (map[string]int64, error) {
	directory, err := s.root.Open(".")
	if err != nil {
		return nil, storage(cannotListLogRootReason, err)
	}
	defer func() { _ = directory.Close() }()
	usage := map[string]int64{}
	for {
		if err := ctx.Err(); err != nil {
			return nil, storage(logOperationCanceledReason, err)
		}
		entries, readErr := directory.ReadDir(128)
		for _, entry := range entries {
			id := ownedRun(entry.Name())
			if id == "" {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				return nil, storage(cannotInspectLogEvidenceReason, err)
			}
			if info.Mode().IsRegular() {
				usage[id] += info.Size()
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, storage(cannotListLogEvidenceReason, readErr)
		}
	}
	return usage, nil
}
func ownedRun(name string) string {
	for _, suffix := range []string{".index.json", ".log"} {
		if strings.HasSuffix(name, suffix) {
			id := strings.TrimSuffix(name, suffix)
			if validID(id) {
				return id
			}
		}
	}
	return ""
}

// Purge removes the owned body and index. C2 fences writers and
// supplies authorization/retention eligibility before calling it. Partial I/O
// failures return the exact bytes already removed along with an actionable error.
func (s *Store) Purge(ctx context.Context, id string) (int64, error) {
	if err := checkID(id); err != nil {
		return 0, err
	}
	if err := s.lock(ctx); err != nil {
		return 0, err
	}
	defer s.unlock()
	names := []string{id + ".log", id + ".index.json"}
	sizes := map[string]int64{}
	for _, name := range names {
		info, err := s.root.Lstat(name)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !info.Mode().IsRegular() {
			return 0, storage(logEvidenceMustBeARegularFileReason, err)
		}
		sizes[name] = info.Size()
	}
	var removed int64
	for _, name := range names {
		if size, ok := sizes[name]; ok {
			if err := ctx.Err(); err != nil {
				return removed, storage(logOperationCanceledReason, err)
			}
			if err := s.root.Remove(name); err != nil {
				return removed, storage(logPurgeStoppedReason, err)
			}
			removed += size
		}
	}
	s.infrastructureHealth.purge(id)
	return removed, nil
}
