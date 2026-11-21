package service_test

import (
	"context"
	"sync"
	"time"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
)

type testMovingGit struct {
	Remote  func(context.Context, string, string) (gitport.Ref, error)
	Prepare func(context.Context, gitport.PromotionRequest) (gitport.Promotion, error)
	Push    func(context.Context, model.Publication) (gitport.PublicationReceipt, error)
}

func (f *testMovingGit) RemoteRef(ctx context.Context, repo, ref string) (gitport.Ref, error) {
	return f.Remote(ctx, repo, ref)
}
func (f *testMovingGit) PreparePromotion(ctx context.Context, q gitport.PromotionRequest) (gitport.Promotion, error) {
	return f.Prepare(ctx, q)
}
func (f *testMovingGit) Publish(ctx context.Context, p model.Publication) (gitport.PublicationReceipt, error) {
	return f.Push(ctx, p)
}

type testHolds struct {
	mu      sync.Mutex
	live    map[string][]model.LiveHold
	Listing func(context.Context, stateport.Run) ([]model.LiveHold, error)
	Stop    func(context.Context, stateport.Run, model.LiveHold) error
	Execute func(context.Context, stateport.Run, model.LiveHold, apicontract.RunHoldExecArgs) (apicontract.ExecResult, error)
}

func (f *testHolds) setLive(id string, rows []model.LiveHold) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.live == nil {
		f.live = map[string][]model.LiveHold{}
	}
	f.live[id] = append([]model.LiveHold{}, rows...)
}
func (f *testHolds) List(ctx context.Context, r stateport.Run) ([]model.LiveHold, error) {
	if f.Listing != nil {
		return f.Listing(ctx, r)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]model.LiveHold{}, f.live[r.ID]...), nil
}
func (f *testHolds) Abandon(ctx context.Context, r stateport.Run, h model.LiveHold) error {
	return f.Stop(ctx, r, h)
}
func (f *testHolds) Exec(ctx context.Context, r stateport.Run, h model.LiveHold, q apicontract.RunHoldExecArgs) (apicontract.ExecResult, error) {
	return f.Execute(ctx, r, h, q)
}

type testTimers struct {
	mu   sync.Mutex
	jobs []*timer
}
type timer struct {
	owner   *testTimers
	delay   time.Duration
	call    func()
	stopped bool
}

func (t *timer) Stop() bool {
	t.owner.mu.Lock()
	defer t.owner.mu.Unlock()
	old := t.stopped
	t.stopped = true
	return !old
}
func (f *testTimers) After(delay time.Duration, call func()) service.Timer {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := &timer{owner: f, delay: delay, call: call}
	f.jobs = append(f.jobs, t)
	return t
}
func (f *testTimers) Fire() bool {
	f.mu.Lock()
	var call func()
	for _, t := range f.jobs {
		if !t.stopped {
			t.stopped = true
			call = t.call
			break
		}
	}
	f.mu.Unlock()
	if call == nil {
		return false
	}
	call()
	return true
}
func (f *testTimers) Delays() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []time.Duration{}
	for _, t := range f.jobs {
		if !t.stopped {
			out = append(out, t.delay)
		}
	}
	return out
}
