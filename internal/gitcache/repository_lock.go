package gitcache

import (
	"context"

	"golang.org/x/sync/semaphore"
)

// One fair queue admits concurrent readers or one exclusive ref/cache mutation.
// Weighted acquisition lets cancelled waiters leave without detached goroutines.
const repositoryReaders int64 = 1 << 30

type repositoryLock struct{ queue *semaphore.Weighted }

func newRepositoryLock() *repositoryLock {
	return &repositoryLock{queue: semaphore.NewWeighted(repositoryReaders)}
}
func (l *repositoryLock) Lock() { _ = l.LockContext(context.Background()) }
func (l *repositoryLock) LockContext(ctx context.Context) error {
	return l.queue.Acquire(ctx, repositoryReaders)
}
func (l *repositoryLock) TryLock() bool                          { return l.queue.TryAcquire(repositoryReaders) }
func (l *repositoryLock) Unlock()                                { l.queue.Release(repositoryReaders) }
func (l *repositoryLock) RLockContext(ctx context.Context) error { return l.queue.Acquire(ctx, 1) }
func (l *repositoryLock) TryRLock() bool                         { return l.queue.TryAcquire(1) }
func (l *repositoryLock) RUnlock()                               { l.queue.Release(1) }

func (l *repositoryLock) downgrade() { l.queue.Release(repositoryReaders - 1) }
