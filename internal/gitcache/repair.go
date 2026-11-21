package gitcache

import (
	"context"
	"errors"
	"os"
	"path/filepath"
)

// PendingReceive exposes the original durable evidence for an authorized repair.
func (c *Cache) PendingReceive(ctx context.Context, repo string) (Reservation, error) {
	p, err := c.path(repo)
	if err != nil {
		return Reservation{}, err
	}
	if err = c.lockMirror(ctx, p); err != nil {
		return Reservation{}, err
	}
	defer c.unlock(p)
	r, _, err := readReceiveRepair(p, repo)
	if files := c.receiveCleanupFor(p); files != nil && !c.isActive(files.owner) && files.rejected {
		r.State = "rejected"
	}
	c.activeMu.Lock()
	r.Problem = c.replayProblems[p]
	c.activeMu.Unlock()
	return r, err
}

// AbandonReceive follows C2's durable audited decision. It removes only the
// named event and its own retention. Valid accepted refs remain untouched;
// corrupt records restore complete predecessor evidence before removal.
func (c *Cache) AbandonReceive(ctx context.Context, repo, event string) error {
	p, err := c.path(repo)
	if err != nil {
		return err
	}
	if err = acquireContext(ctx, c.repositoryGate(p+"/receive")); err != nil {
		return err
	}
	defer func() { <-c.repositoryGate(p + "/receive") }()
	return c.abandonReceive(ctx, p, repo, event)
}

func (c *Cache) abandonReceive(ctx context.Context, p, repo, event string) error {
	if err := c.lockMirror(ctx, p); err != nil {
		return err
	}
	defer c.unlock(p)
	r, owner, err := readReceiveRepair(p, repo)
	if err != nil {
		return err
	}
	if event == "" || r.Event.ID != event {
		return refuse("receive event identity")
	}
	return c.abandonReceiveRecord(ctx, p, r, owner)
}

func (c *Cache) abandonReceiveRecord(ctx context.Context, p string, r Reservation, owner string) error {
	var err error
	files := c.receiveCleanupFor(p)
	rejected := r.State == "rejected"
	if files != nil {
		if err = files.cleanReceive(); err != nil {
			return err
		}
		rejected = rejected || files.rejected
	}
	if r.State != "corrupt" {
		r, _, err = c.settleReceiveRecord(ctx, p, r, rejected)
		if err != nil {
			return err
		}
	} else if r.Before != nil {
		if err = c.rollbackReceiveRefs(ctx, p, r.Before); err != nil {
			return &receiveRollbackError{unavailable("restore abandoned receive", err)}
		}
	}
	if r.Event.ID == "" {
		c.forgetReceiveFiles(p, files)
		return nil
	}
	if owner != "" {
		err = c.finishReceiveLocked(ctx, p, owner)
	} else {
		err = removeReservation(p)
	}
	if err == nil {
		c.forgetReceiveFiles(p, files)
		c.recordReplayProblem(p, nil)
	}
	return err
}
func removeReservation(p string) error {
	if err := os.Remove(filepath.Join(p, receiveRecordFile)); err != nil {
		return err
	}
	d, err := os.Open(p)
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}
