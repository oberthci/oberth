package service_test

import (
	"context"
	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/service"
	statefake "github.com/oberthci/oberth/internal/stateport/fake"
	"maps"
	"sync"
	"testing"
	"time"
)

// fixtureStore decorates owner ports with authority, counters and injected failures.
// All run, catalogue, publication, promotion and hold state lives in native.
type fixtureStore struct {
	service.RunStore
	service.CatalogStore
	service.EventStore
	service.PublicationStore
	service.PromotionStore
	service.HoldStore
	native                       *statefake.MovingRuns
	mu                           sync.Mutex
	Actors                       map[string]actor.Actor
	Policies                     map[string]service.KeyPolicy
	Err, WriteError, PolicyError error
	calls                        map[string]int
}

func newFixtureStore(clock func() time.Time) *fixtureStore {
	n := statefake.WithMoving(statefake.NewRuns(clock))
	f := &fixtureStore{RunStore: n, CatalogStore: n, EventStore: n, PublicationStore: n, PromotionStore: n, HoldStore: n, native: n, Actors: map[string]actor.Actor{}, Policies: map[string]service.KeyPolicy{}, calls: map[string]int{}}
	n.Before = func(name string, write bool) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls[name]++
		if write && f.WriteError != nil {
			return f.WriteError
		}
		return f.Err
	}
	return f
}
func fixtureProblem() error {
	return publicerr.New(publicerr.NotFound, "The fixture identity was not found.", "Use a registered fixture identity.", nil)
}
func (f *fixtureStore) Current(ctx context.Context, a actor.Actor) (actor.Actor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls["Current"]++
	if err := ctx.Err(); err != nil {
		return actor.Actor{}, err
	}
	if f.Err != nil {
		return actor.Actor{}, f.Err
	}
	current, ok := f.Actors[a.Fingerprint]
	if !ok {
		return current, fixtureProblem()
	}
	return current.Clone(), nil
}
func (f *fixtureStore) KeyPolicy(ctx context.Context, a actor.Actor) (service.KeyPolicy, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls["KeyPolicy"]++
	if err := ctx.Err(); err != nil {
		return service.KeyPolicy{}, err
	}
	if f.PolicyError != nil {
		return service.KeyPolicy{}, f.PolicyError
	}
	policy, ok := f.Policies[a.Fingerprint]
	if !ok {
		return policy, fixtureProblem()
	}
	return policy, f.Err
}
func (f *fixtureStore) Calls() map[string]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return maps.Clone(f.calls)
}
func (f *fixture) archiveRepository(t *testing.T, name string) {
	t.Helper()
	r, err := f.store.Repository(context.Background(), name, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Archived = true
	f.store.native.SeedRepository(r)
}
