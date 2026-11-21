package gitcache

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// receiveRecordFile is the durable record of a receive that is not settled yet.
const receiveRecordFile = "oberth-receive.json"

// receivePending reports whether a receive owns the store's public refs. A
// refresh, a publication's cache record and an archive all leave them alone
// until it is settled; an unreadable record counts as pending.
func receivePending(store string) bool {
	_, err := os.Stat(filepath.Join(store, receiveRecordFile))
	return !os.IsNotExist(err)
}

// PendingReceives includes orphaned repositories so repair never needs registration.
func (c *Cache) PendingReceives(ctx context.Context) ([]Reservation, error) {
	paths, err := c.repositories()
	if err != nil {
		return nil, err
	}
	var records []Reservation
	for _, p := range paths {
		relative, err := filepath.Rel(c.config.Root, p)
		if err != nil {
			return nil, err
		}
		repo := strings.TrimSuffix(filepath.ToSlash(relative), ".git")
		r, err := c.PendingReceive(ctx, repo)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		records = append(records, r)
	}
	return records, nil
}

func (c *Cache) recordReplayProblem(p string, err error) {
	c.activeMu.Lock()
	defer c.activeMu.Unlock()
	if err == nil {
		delete(c.replayProblems, p)
		return
	}
	if c.replayProblems == nil {
		c.replayProblems = make(map[string]string)
	}
	var pending *deliveryPendingError
	if errors.As(err, &pending) {
		err = pending.Unwrap()
	}
	reason := "receive delivery is unavailable"
	var problem interface{ Reason() string }
	if errors.As(err, &problem) {
		reason = problem.Reason()
	}
	c.replayProblems[p] = reason
}
