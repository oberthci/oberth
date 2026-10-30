package gitcache

import (
	"context"

	"golang.org/x/sync/semaphore"
)

// repositoryLock shares one FIFO queue between ordinary and deadline-aware
// callers. TryLock also respects queued waiters (notably TagObjectSHA).
type repositoryLock struct{ queue *semaphore.Weighted }

func newRepositoryLock() *repositoryLock                        { return &repositoryLock{queue: semaphore.NewWeighted(1)} }
func (l *repositoryLock) Lock()                                 { _ = l.LockContext(context.Background()) }
func (l *repositoryLock) LockContext(ctx context.Context) error { return l.queue.Acquire(ctx, 1) }
func (l *repositoryLock) TryLock() bool                         { return l.queue.TryAcquire(1) }
func (l *repositoryLock) Unlock()                               { l.queue.Release(1) }
