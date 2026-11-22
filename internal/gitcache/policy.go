package gitcache

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/oberthci/oberth/internal/gitport"
)

// policyHead reads the forge's current default head of repo and makes sure
// its object is in the cache. Each forge command runs as a job of the
// repository and its forge identity under the cache lifetime, joined only by
// callers whose registration names the same forge and branch, so a
// caller's deadline never kills it: the head identity, the repository's
// refresh fetch when the cache lacks the head (it tells the cache's readers
// which refs it moved, so the producer's schedules follow), and a fetch of a
// head the refresh did not bring. An object the cache has needs no transfer.
// A caller waits at most the refresh wait within ctx; one that stops waiting
// is told the forge did not answer, or that its copy is still updating once
// the forge is sending.
func (c *Cache) policyHead(ctx context.Context, repo string, r Remote) (gitport.Ref, error) {
	p, err := c.path(repo)
	if err != nil {
		return gitport.Ref{}, err
	}
	wait, cancel := context.WithTimeout(ctx, c.budgets.refreshWait)
	defer cancel()
	store := c.root()
	head, err := c.forgeJob(wait, forgeJobKey(p, "head", r, ""), func(ctx context.Context, _ *atomic.Bool) outcome {
		head, err := store.remoteRef(ctx, p, r, r.DefaultBranch)
		if isAbsent(err) {
			err = store.policyAbsence(ctx, p, r)
		}
		return outcome{head: head, err: err}
	})
	if err != nil || c.has(ctx, p, head.ObjectSHA) {
		return head, err
	}
	var updating *gitport.FetchUnfinishedError
	if err = c.refresh(wait, repo, false); errors.As(err, &updating) {
		return head, err
	}
	if c.has(ctx, p, head.ObjectSHA) {
		return head, nil
	}
	_, err = c.forgeJob(wait, forgeJobKey(p, "object", r, head.ObjectSHA), func(ctx context.Context, sending *atomic.Bool) outcome {
		return outcome{err: store.fetchHeadObject(ctx, p, r, fetchedObject{oid: head.ObjectSHA, sending: sending})}
	})
	return head, err
}

// root is the cache whose store a forge job writes: a receive-scoped copy's
// jobs outlive the receive, so they never run in its quarantine.
func (c *Cache) root() *Cache {
	if c.unscoped != nil {
		return c.unscoped
	}
	return c
}

// forgeJob joins or starts the forge job of key and waits for it within ctx.
// A caller that stops waiting leaves the job running.
func (c *Cache) forgeJob(ctx context.Context, key string, run func(context.Context, *atomic.Bool) outcome) (gitport.Ref, error) {
	job := c.root().joinTransfer(key, run, func(context.Context, outcome, ending) {}) //nolint:contextcheck // The cache lifetime owns the job, not the caller who waits for it.
	result, ended := c.root().awaitTransfer(ctx, key, job)
	if !ended {
		return gitport.Ref{}, unanswered(ctx, job)
	}
	return result.head, result.err
}

// unanswered is a caller's outcome when it stopped waiting for job: the forge
// copy is still updating once the forge is sending, and otherwise the forge
// did not answer.
func unanswered(ctx context.Context, job *transfer) error {
	if job.sending.Load() {
		return &gitport.FetchUnfinishedError{Cause: context.Cause(ctx)}
	}
	return unavailable("forge", context.Cause(ctx))
}


// has reports whether the cache holds object oid; a failed check counts as
// absent, so the forge is asked.
func (c *Cache) has(ctx context.Context, p, oid string) bool {
	return c.run(ctx, command{dir: p, args: []string{"cat-file", "-e", oid}}) == nil
}

func (c *Cache) policyAbsence(ctx context.Context, p string, r Remote) error {
	data, err := c.capture(
		ctx,
		command{
			dir:    p,
			args:   []string{"ls-remote", "--", r.URL, "refs/heads/*", "refs/tags/*"},
			remote: &r,
		},
		2<<20,
	)
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return &gitport.EmptyRepositoryError{}
	}
	return &RefusalError{reason: reasonDeletedDefault, remedy: remedyDeletedDefault, Field: r.DefaultBranch}
}
func (c *Cache) checkDefault(ctx context.Context, p, repo string, r Remote) error {
	_, err := c.resolve(ctx, p, repo, r.DefaultBranch)
	if isAbsent(err) {
		return c.policyAbsence(ctx, p, r)
	}
	return err
}
