package artifacts

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

type runUsage struct {
	runID    string
	bytes    int64
	modified time.Time
}

func (store *Store) Usage() (int64, error) {
	runs, err := store.usageByRun()
	if err != nil {
		return 0, err
	}
	var total int64
	for _, run := range runs {
		total += run.bytes
	}
	return total, nil
}

func (store *Store) Evict(budget int64) ([]string, error) {
	if budget <= 0 {
		return nil, nil
	}
	runs, err := store.usageByRun()
	if err != nil {
		return nil, err
	}
	var total int64
	for _, run := range runs {
		total += run.bytes
	}
	if total <= budget {
		return nil, nil
	}
	sort.Slice(runs, func(i, j int) bool {
		if runs[i].modified.Equal(runs[j].modified) {
			return runs[i].runID < runs[j].runID
		}
		return runs[i].modified.Before(runs[j].modified)
	})

	var removed []string
	for _, run := range runs {
		if total <= budget {
			break
		}
		path, pathErr := store.runPath(run.runID)
		if pathErr != nil {
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			return removed, fmt.Errorf("artifacts: evict %s: %w", run.runID, err)
		}
		total -= run.bytes
		removed = append(removed, run.runID)
	}
	return removed, nil
}

func (store *Store) usageByRun() ([]runUsage, error) {
	entries, err := os.ReadDir(store.directory)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("artifacts: read store: %w", err)
	}
	var runs []runUsage
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			continue
		}
		var bytes int64
		root := filepath.Join(store.directory, entry.Name())
		walkErr := filepath.Walk(root, func(_ string, file os.FileInfo, err error) error {
			if err != nil {
				return nil //nolint:nilerr
			}
			if file.Mode().IsRegular() {
				bytes += file.Size()
			}
			return nil
		})
		if walkErr != nil {
			continue
		}
		runs = append(runs, runUsage{runID: entry.Name(), bytes: bytes, modified: info.ModTime()})
	}
	return runs, nil
}
