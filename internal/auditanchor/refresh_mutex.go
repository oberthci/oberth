package auditanchor

import (
	"context"
	"sync"
)

// refreshMutex preserves serialized continuity updates while allowing a gate
// to leave the refresh queue when its total deadline expires. Zero value works.
type refreshMutex struct {
	once  sync.Once
	token chan struct{}
}

func (m *refreshMutex) LockContext(ctx context.Context) error {
	m.once.Do(func() { m.token = make(chan struct{}, 1); m.token <- struct{}{} })
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-m.token:
		if err := ctx.Err(); err != nil {
			m.Unlock()
			return err
		}
		return nil
	}
}
func (m *refreshMutex) Lock()   { _ = m.LockContext(context.Background()) }
func (m *refreshMutex) Unlock() { m.token <- struct{}{} }
