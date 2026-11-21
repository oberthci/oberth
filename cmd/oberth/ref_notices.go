package main

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
)

// refNoticeBound bounds the schedule reconciliation one ref notice starts.
const refNoticeBound = 2 * time.Minute

// repositoryAttempt bounds each repository whenever every repository is
// reconciled (the startup pass after readiness, or a notice naming a
// repository the catalogue no longer knows): room for a 15-second refresh
// plus admission, so one slow repository or first clone cannot hold the
// others. A first clone it gives up on continues and announces its refs.
const repositoryAttempt = 30 * time.Second

// refNotices hands the cache's moved refs to schedule reconciliation outside
// the call that saw them: the reader returns at once. A repository has at most
// one pending notice, which merges the refs of every notice not yet taken,
// and at most one worker, which reconciles under the server's lifetime with
// refNoticeBound per reconciliation and exits when nothing is pending. So the
// workers are bounded by the repositories with a move in flight, and close
// ends them.
type refNotices struct {
	lifetime  context.Context
	stop      context.CancelFunc
	reconcile func(context.Context, string, []string) error
	notice    func(error)
	mu        sync.Mutex
	pending   map[string][]string
	running   map[string]bool
	idle      map[string]chan struct{}
	workers   sync.WaitGroup
}

func newRefNotices(lifetime context.Context, reconcile func(context.Context, string, []string) error, notice func(error)) *refNotices {
	ctx, stop := context.WithCancel(lifetime)
	return &refNotices{lifetime: ctx, stop: stop, reconcile: reconcile, notice: notice, pending: map[string][]string{}, running: map[string]bool{}, idle: map[string]chan struct{}{}}
}

// changed is the cache's RefChanged: it records the moved refs and returns.
func (n *refNotices) changed(_ context.Context, repo string, updates []gitcache.RefUpdate) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.lifetime.Err() != nil {
		return
	}
	refs := n.pending[repo]
	for _, u := range updates {
		if !slices.Contains(refs, u.Ref) {
			refs = append(refs, u.Ref)
		}
	}
	n.pending[repo] = refs
	if !n.running[repo] {
		n.running[repo] = true
		n.workers.Go(func() { n.drain(repo) }) //nolint:contextcheck // The server's lifetime owns the reconciliation, not the reader's call.
	}
}

func (n *refNotices) drain(repo string) {
	for {
		n.mu.Lock()
		refs, found := n.pending[repo]
		delete(n.pending, repo)
		if !found || n.lifetime.Err() != nil {
			delete(n.running, repo)
			if idle := n.idle[repo]; idle != nil {
				close(idle)
				delete(n.idle, repo)
			}
			n.mu.Unlock()
			return
		}
		n.mu.Unlock()
		ctx, cancel := context.WithTimeout(n.lifetime, refNoticeBound)
		n.notice(n.reconcile(ctx, repo, refs))
		cancel()
	}
}

// settled waits, within ctx, until repo has no move pending or in
// reconciliation.
func (n *refNotices) settled(ctx context.Context, repo string) error {
	n.mu.Lock()
	if !n.running[repo] {
		n.mu.Unlock()
		return nil
	}
	idle := n.idle[repo]
	if idle == nil {
		idle = make(chan struct{})
		n.idle[repo] = idle
	}
	n.mu.Unlock()
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// close ends every reconciliation in flight and waits for its worker. The
// lifetime ends under the mutex, so a notice either started its worker before
// the wait or sees the ended lifetime and starts none.
func (n *refNotices) close(ctx context.Context) error {
	n.mu.Lock()
	n.stop()
	n.mu.Unlock()
	done := make(chan struct{})
	go func() { n.workers.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
