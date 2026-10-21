package gitcache

import (
	"context"
	"errors"
	"fmt"
	"os"
)

var ErrArchiveCacheCleanup = errors.New("archived repository cache cleanup incomplete")
var ErrArchivePendingReceive = errors.New("repository has a pending receive")

func (c *Cache) checkRepositoryActive(ctx context.Context, input string) error {
	if c.repositoryGate != nil {
		return c.repositoryGate(ctx, input)
	}
	return nil
}

// ChangeRepositoryArchive serializes the catalog transition and cache removal
// with receives and refreshes. Identity resolution remains available while
// archived, but every cache refresh rechecks the catalog under this same lock.
func (c *Cache) ChangeRepositoryArchive(ctx context.Context, input string, archived bool, change func() error) error {
	repo, path, err := c.path(input)
	if err != nil {
		return err
	}
	receiveLock := c.receiveLock(path)
	receiveLock.Lock()
	defer receiveLock.Unlock()
	lock := c.repoLock(path)
	lock.Lock()
	defer lock.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if archived {
		if _, err := c.readReservation(c.reservationIdentity(repo, path)); err == nil {
			return fmt.Errorf("%w: recover it before archiving", ErrArchivePendingReceive)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("check pending receive before archive: %w", err)
		}
	}
	if err := change(); err != nil {
		return err
	}
	if archived {
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("%w: repository archived; retry archive: %w", ErrArchiveCacheCleanup, err)
		}
	}
	return nil
}
