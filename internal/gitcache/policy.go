package gitcache

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/oberthci/oberth/internal/gitport"
)

// policyHead reads the forge's current default head of repo and makes sure
// its object is in the cache, for the reads that are not admission decisions
// (a publication's predecessor, the promotion policy, a default-ref
// checkout). Each forge command runs as a job of the repository and its forge
// identity under the cache lifetime, joined only by callers whose
// registration names the same forge and branch, so a caller's deadline never
// kills it. A caller waits at most the refresh wait within ctx; one that stops
// waiting is told the forge did not answer, or that its copy is still
// updating once the forge is sending. An absent default branch is told from
// an empty forge within the same job, so an unfinished listing is an outage.
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
	if err != nil {
		return head, err
	}
	return head, c.headObject(ctx, wait, r, head)
}

// admissionRead reads r's default head for an admission decision within the
// head wait: the forge's answer is kept (headRead) for a retry whose own read
// is slow, and an answer that the default branch is absent stands although
// the listing that tells an empty forge from a deleted branch did not finish
// (absence). The cache is read under ctx, the forge waited for at most the
// head wait.
func (c *Cache) admissionRead(ctx context.Context, p string, r Remote) (gitport.Ref, error) {
	wait, cancel := context.WithTimeout(ctx, c.budgets.headWait)
	defer cancel()
	head, err := c.root().headRead(wait, p, r)
	if isAbsent(err) {
		return head, c.absence(wait, p, r, err)
	}
	if err != nil {
		return head, err
	}
	return head, c.headObject(ctx, wait, r, head)
}

// headObject makes sure the object of the forge's head is in the cache: an
// object the cache has needs no transfer; otherwise the repository's refresh
// fetch (it tells the cache's readers which refs it moved, so the producer's
// schedules follow) and then a fetch of a head the refresh did not bring.
// The cache is read under ctx, the forge waited for within wait.
func (c *Cache) headObject(ctx, wait context.Context, r Remote, head gitport.Ref) error {
	p, err := c.path(r.repository)
	if err != nil || c.has(ctx, p, head.ObjectSHA) {
		return err
	}
	var updating *gitport.FetchUnfinishedError
	if err = c.refresh(wait, r.repository, false); errors.As(err, &updating) {
		return err
	}
	if c.has(ctx, p, head.ObjectSHA) {
		return nil
	}
	_, err = c.forgeJob(wait, forgeJobKey(p, "object", r, head.ObjectSHA), c.root().objectRun(p, r, head.ObjectSHA))
	return err
}

// headRead reads r's default head for an admission decision as a job of its
// forge identity and keeps the answer (headAnswers). A caller whose wait ends first decides from the
// answer kept within cacheTrustWindow, as from one the forge just gave, and
// otherwise is told the forge did not answer. A read that ends after every
// caller left starts the fetch of a head object the store lacks, so a retry
// finds it. c is the root cache.
func (c *Cache) headRead(ctx context.Context, p string, r Remote) (gitport.Ref, error) {
	key := forgeJobKey(p, "admission-head", r, "")
	run := func(ctx context.Context, _ *atomic.Bool) outcome {
		generation := c.answers.generation(p)
		head, err := c.remoteRef(ctx, p, r, r.DefaultBranch)
		c.answers.record(headRecord{key: key, store: p, generation: generation, at: c.config.Now()}, head, err)
		return outcome{head: head, err: err}
	}
	ended := func(ctx context.Context, result outcome, end ending) {
		if result.err == nil && end.unattended && !c.has(ctx, p, result.head.ObjectSHA) {
			object := forgeJobKey(p, "object", r, result.head.ObjectSHA)
			c.leave(object, c.joinTransfer(object, c.objectRun(p, r, result.head.ObjectSHA), noEnd)) //nolint:contextcheck // The cache lifetime owns the fetch; no caller waits for it.
		}
	}
	job := c.joinTransfer(key, run, ended) //nolint:contextcheck // The cache lifetime owns the job, not the caller who waits for it.
	if c.abandoned(job) {
		// An earlier caller already waited for this read in vain: the forge
		// is not answering in time, so later callers (the other pins of the
		// same admission, a retry) decide at once instead of waiting again.
		var expired context.CancelFunc
		ctx, expired = context.WithTimeout(ctx, 0)
		defer expired()
	}
	if result, ended := c.awaitTransfer(ctx, key, job); ended {
		return result.head, result.err
	}
	if kept, ok := c.answers.recent(key, c.config.Now(), c.lastRefresh(p)); ok {
		return kept, nil
	}
	return gitport.Ref{}, unanswered(ctx, job)
}

// objectRun fetches the forge's head object oid into the store at p.
func (c *Cache) objectRun(p string, r Remote, oid string) func(context.Context, *atomic.Bool) outcome {
	return func(ctx context.Context, sending *atomic.Bool) outcome {
		return outcome{err: c.fetchHeadObject(ctx, p, r, fetchedObject{oid: oid, sending: sending})}
	}
}

func noEnd(context.Context, outcome, ending) {}

// absence follows a forge that answered that the default branch is absent
// with the listing that tells an empty forge from a deleted default branch.
// The forge has answered: a listing that does not finish or fails keeps that
// answer, so no caller decides from the cache.
func (c *Cache) absence(ctx context.Context, p string, r Remote, absent error) error {
	store := c.root()
	_, err := c.forgeJob(ctx, forgeJobKey(p, "absence", r, ""), func(ctx context.Context, _ *atomic.Bool) outcome {
		return outcome{err: store.policyAbsence(ctx, p, r)}
	})
	if forgeError(err) {
		return absent
	}
	return err
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
	job := c.root().joinTransfer(key, run, noEnd) //nolint:contextcheck // The cache lifetime owns the job, not the caller who waits for it.
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
