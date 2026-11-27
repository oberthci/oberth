// Package service records attributed acceptance and native facts and serves scoped run history.
// Wiring owns preparation, submission and recovery.
// Tool handlers consume schema-validated apicontract arguments.
//
// Spec: docs/plans/c2-service-actions-and-history.md#purpose
package service

import (
	"context"
	"sync"
	"time"
)

type Config struct {
	People       People
	Publications PublicationStore
	Promotions   PromotionStore
	MovingGit    MovingGit
	HoldStore    HoldStore
	Holds        HoldExec
	AfterFunc    AfterFunc
	NewID        func() string
	OnError      func(error)
	Runs         RunStore
	Authority    AuthorityStore
	Catalog      CatalogStore
	Events       EventStore
	Git          Git
	Source       Source
	Exec         Exec
	Starter      Starter
	Logs         Logs
	Now          func() time.Time
}
type Service struct {
	gates   map[string]*gate
	timers  map[string]Timer
	closing bool
	backup  sync.Mutex
	tasks   sync.WaitGroup
	c       Config
	ctx     context.Context
	cancel  context.CancelFunc
	mu      sync.Mutex
	scans   map[string]chan struct{}
	slots   chan struct{}
}

func New(ctx context.Context, c Config) (*Service, error) {
	if c.Now == nil || c.AfterFunc == nil || c.NewID == nil || c.OnError == nil {
		return nil, refuse(unavailable)
	}
	for _, port := range []any{c.Publications, c.Promotions, c.MovingGit, c.HoldStore, c.Holds, c.Runs, c.Authority, c.Catalog, c.Events, c.Git, c.Source, c.Exec, c.Starter, c.Logs} {
		if port == nil {
			return nil, refuse(unavailable)
		}
	}
	if err := c.People.validate(); err != nil {
		return nil, err
	}
	if err := c.People.ReleaseRoutes.Validate(); err != nil {
		return nil, err
	}
	c.People.ReleaseRoutes = c.People.ReleaseRoutes.Clone()
	life, cancel := context.WithCancel(ctx)
	return &Service{gates: map[string]*gate{}, timers: map[string]Timer{}, c: c, ctx: life, cancel: cancel, scans: make(map[string]chan struct{}), slots: make(chan struct{}, 2)}, nil
}
func (s *Service) Shutdown(ctx context.Context) error { return s.stopMoving(ctx) }

// bounded keeps a prepared short call within the service's read/effect budget.
func bounded[T any](call func(context.Context) (T, error)) func(context.Context) (T, error) {
	return func(ctx context.Context) (T, error) {
		var zero T
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		read, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		return call(read)
	}
}
