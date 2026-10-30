package runlog

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var evidenceSuffixes = []string{".log", ".index.json", ".progress.jsonl", ".plan.json"}

// UsageByRun includes indexes and progress journals as well as log bodies.
func (store *Store) UsageByRun() (map[string]int64, error) {
	entries, err := os.ReadDir(store.directory)
	if err != nil {
		return nil, err
	}
	usage := make(map[string]int64)
	for _, entry := range entries {
		for _, suffix := range evidenceSuffixes {
			id, ok := strings.CutSuffix(entry.Name(), suffix)
			if !ok || !safeID(id) {
				continue
			}
			info, err := entry.Info()
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, err
			}
			if info.Mode().IsRegular() {
				usage[id] += info.Size()
			}
			break
		}
	}
	return usage, nil
}

func (store *Store) Purge(runID string) (int64, error) {
	store.ForgetActiveIndex(runID)
	defer store.ForgetActiveIndex(runID)
	if _, err := store.logPath(runID); err != nil {
		return 0, err
	}
	store.progressMu.Lock()
	defer store.progressMu.Unlock()
	var removed int64
	for _, suffix := range evidenceSuffixes {
		path := filepath.Join(store.directory, runID+suffix)
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return removed, err
		}
		if !info.Mode().IsRegular() {
			return removed, fmt.Errorf("refuse nonregular run log evidence %s", runID)
		}
		if err := os.Remove(path); err != nil {
			return removed, err
		}
		removed += info.Size()
	}
	return removed, nil
}
