package gitcache

import (
	"context"
	"errors"
	"fmt"
	"github.com/oberthci/oberth/internal/sourcedelivery"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Root ownership excludes other server processes before recovery touches Git
// locks. Repository gates serialize only operations on the same store.
type repositoryGates struct {
	mu         sync.Mutex
	kept       map[string]uint64
	sequence   uint64
	sweeps     int
	repos      map[string]chan struct{}
	publishing map[string]bool
}

func (c *Cache) repositoryGate(p string) chan struct{} {
	c.gates.mu.Lock()
	defer c.gates.mu.Unlock()
	gate := c.gates.repos[p]
	if gate == nil {
		gate = make(chan struct{}, 1)
		c.gates.repos[p] = gate
	}
	return gate
}
func (c *Cache) lock(ctx context.Context, p string) error {
	_, _, err := c.lockReceive(ctx, p, false)
	return err
}

// An out-of-band ref mismatch requires repair but does not invalidate existing source.
func (c *Cache) lockRead(ctx context.Context, p string) error {
	_, _, err := c.lockReceive(ctx, p, true)
	return err
}

// Return the receive decision made under this lock so replay does not inspect twice.
func (c *Cache) lockReceive(ctx context.Context, p string, readOnly bool) (Reservation, []string, error) {
	if err := c.lockMirror(ctx, p); err != nil {
		return Reservation{}, nil, err
	}
	record, lines, err := c.restoreUnchecked(ctx, p)
	if err != nil {
		if refusal := receiveRefusal(err); refusal != nil && c.config.Log != nil {
			c.config.Log(refusal.Reason(), err.Error())
		}
		var mismatch *RefusalError
		if readOnly && errors.As(err, &mismatch) && mismatch.Field == "receive recovery mismatch" {
			return record, lines, nil
		}
		c.unlock(p)
		return Reservation{}, lines, fmt.Errorf("repository %s: %w", strings.TrimSuffix(strings.TrimPrefix(p,
			c.config.Root+string(filepath.Separator)), ".git"), err)
	}
	return record, lines, nil
}

// Mirror recovery precedes both ordinary operations and authorized receive repair.
func (c *Cache) lockMirror(ctx context.Context, p string) error {
	slot := c.repositoryGate(p)
	select {
	case slot <- struct{}{}:
	default:
		end := sourcedelivery.Measure(ctx, sourcedelivery.Repository, true)
		err := acquireContext(ctx, slot)
		end()
		if err != nil {
			return err
		}
	}
	defer sourcedelivery.Measure(ctx, sourcedelivery.Recovery, false)()
	if err := ctx.Err(); err != nil {
		c.unlock(p)
		return err
	}
	err := c.reconcileMirror(ctx, p)
	if err != nil {
		err = c.setAsideReconciliation(p, err)
	}
	if err != nil {
		c.unlock(p)
	}
	return err
}
func (c *Cache) unlock(p string) { <-c.repositoryGate(p) }

func ownRoot(root string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(root, "oberth.owner"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = claimRoot(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func (c *Cache) keptDuringGC(owner string, epoch uint64) bool {
	c.gates.mu.Lock()
	defer c.gates.mu.Unlock()
	return c.gates.kept[owner] > epoch
}
func (c *Cache) noteKeep(owner string) {
	c.gates.mu.Lock()
	defer c.gates.mu.Unlock()
	if c.gates.kept != nil {
		c.gates.sequence++
		c.gates.kept[owner] = c.gates.sequence
	}
}

func (c *Cache) publicationBusy(p string) bool {
	c.gates.mu.Lock()
	defer c.gates.mu.Unlock()
	for key := range c.gates.publishing {
		if strings.HasPrefix(key, p+"\x00") {
			return true
		}
	}
	return false
}
func (c *Cache) publicationRefBusy(p, ref string) bool {
	c.gates.mu.Lock()
	defer c.gates.mu.Unlock()
	return c.gates.publishing[p+"\x00"+ref]
}
func (c *Cache) receivePublicationBusy(p string) bool {
	r, err := readReservation(p)
	if err != nil {
		return c.publicationBusy(p)
	}
	for _, u := range r.Event.Updates {
		if c.publicationRefBusy(p, u.Ref) {
			return true
		}
	}
	return false
}
func (c *Cache) setPublication(p string, active bool) {
	c.gates.mu.Lock()
	defer c.gates.mu.Unlock()
	if c.gates.publishing == nil {
		c.gates.publishing = make(map[string]bool)
	}
	if active {
		c.gates.publishing[p] = true
	} else {
		delete(c.gates.publishing, p)
	}
}

func (c *Cache) beginCollection() uint64 {
	c.gates.mu.Lock()
	defer c.gates.mu.Unlock()
	c.gates.sequence++
	c.gates.sweeps++
	if c.gates.kept == nil {
		c.gates.kept = make(map[string]uint64)
	}
	return c.gates.sequence
}
func (c *Cache) endCollection() {
	c.gates.mu.Lock()
	defer c.gates.mu.Unlock()
	c.gates.sweeps--
	if c.gates.sweeps == 0 {
		c.gates.kept = nil
	}
}

func cleanMaterializations(root string) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if strings.HasSuffix(d.Name(), ".git.staging") {
			if err = os.RemoveAll(p); err != nil {
				return err
			}
			return filepath.SkipDir
		}
		if strings.HasSuffix(d.Name(), ".git") {
			if err = cleanGitLocks(p); err != nil {
				return err
			}
			return filepath.SkipDir
		}
		return nil
	})
}

// Exclusive startup only: root ownership proves that no old native worker lives.
func cleanGitLocks(store string) error {
	if err := (commandFiles{filepath.Join(store, "packed-refs.new")}).clean(); err != nil {
		return err
	}
	return filepath.WalkDir(store, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() &&
			strings.HasPrefix(d.Name(), "tmp_objdir-incoming-") &&
			filepath.Dir(p) == filepath.Join(store, "objects") {
			if err = os.RemoveAll(p); err != nil {
				return err
			}
			return filepath.SkipDir
		}
		if !d.IsDir() && (strings.HasSuffix(d.Name(), ".lock") ||
			filepath.Dir(p) == store && strings.HasPrefix(d.Name(), recordTempPrefix)) {
			return os.Remove(p)
		}
		return nil
	})
}
