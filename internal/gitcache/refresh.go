package gitcache

import (
	"context"
	"errors"
	"fmt"
	"github.com/oberthci/oberth/internal/gitport"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
)

// RefreshInterval bounds how long demand reads reuse a forge snapshot or failure.
const RefreshInterval = 45 * time.Second

// refreshDeadline is how long a caller waits for the repository's forge fetch
// before it reads the cache or is told the fetch continues; the fetch itself
// runs to the background bound.
const refreshDeadline = 15 * time.Second
const refreshFailureFile = "oberth-refresh-failed"
const lookFile = "oberth-looked"

// refresh brings the forge's refs into the cache when its last refresh is older
// than RefreshInterval, and tells the cache's readers (RefChanged) which public
// refs that moved, whichever read caused it. Schedule reconciliation, the
// reader of those notices, refreshes quietly: a notice from inside it would
// wait for the reconciliation that sent it.
func (c *Cache) refresh(ctx context.Context, repo string, force bool) error {
	updates, err := c.refreshQuietly(ctx, repo, force)
	if err == nil {
		c.changed(ctx, repo, updates)
	}
	return err
}

// refreshQuietly waits at most refreshDeadline, within ctx, for the
// repository's forge fetch, starting one when none runs. The fetch runs under
// the cache lifetime and the background bound, one per repository at a time,
// so a fetch with much to transfer finishes even when no caller waits that
// long. A caller that stops waiting is told the forge copy is still updating
// once the forge is sending a pack, and that the forge did not answer while
// it sends none; a fetch every caller left announces the refs it moved itself.
func (c *Cache) refreshQuietly(ctx context.Context, repo string, force bool) ([]RefUpdate, error) {
	if c.unscoped != nil {
		return c.unscoped.refreshQuietly(ctx, repo, force)
	}
	if c.config.Upstream == nil {
		return nil, nil
	}
	p, err := c.path(repo)
	if err != nil {
		return nil, err
	}
	if !force {
		if recent, err := c.recentRefresh(p); recent {
			return nil, err
		}
	}
	wait, cancel := context.WithTimeout(ctx, c.budgets.refreshWait)
	defer cancel()
	key := p + "/fetch"
	fetch := c.startFetch(repo, p, force) //nolint:contextcheck // The cache lifetime owns the fetch, not the caller who waits for it.
	result, ended := c.awaitTransfer(wait, key, fetch)
	if !ended {
		return nil, unanswered(wait, fetch)
	}
	var refusal *RefusalError
	if !force && errors.As(result.err, &refusal) && refusal.Field == "pending receive" {
		// The pending receive owns the public inventory until it is settled;
		// an ordinary read is served the cache, a forced look learns why.
		return nil, nil
	}
	return result.updates, result.err
}

// startFetch joins or starts the refresh fetch of repository at p. A refresh
// that became recent while the fetch waited to start fetches nothing unless
// forced; a pending receive owns the public inventory until it is settled, so
// the fetch then moves nothing and each caller reads that as its own mode
// says; a forge failure is recorded only from the fetch's own outcome. The refs a fetch moved go to the cache's readers when
// no caller waited to take them, for the repository registered under the
// catalogue ID it started with.
func (c *Cache) startFetch(repo, p string, force bool) *transfer {
	var registered int64
	run := func(ctx context.Context, sending *atomic.Bool) outcome {
		if !force {
			if recent, err := c.recentRefresh(p); recent {
				return outcome{err: err}
			}
		}
		remote, err := c.remote(ctx, repo)
		if err != nil {
			return outcome{err: err}
		}
		registered = remote.RepositoryID
		updates, err := c.fetch(ctx, fetchTarget{store: p, repo: repo, remote: remote, sending: sending})
		return outcome{updates: updates, err: c.recordRefreshResult(p, err)}
	}
	ended := func(ctx context.Context, result outcome, end ending) {
		if result.err == nil && len(result.updates) > 0 && end.unattended && sameRepository(registered, c.registeredID(ctx, repo)) {
			c.changed(ctx, repo, result.updates)
		}
	}
	return c.joinTransfer(p+"/fetch", run, ended)
}

func (c *Cache) recentRefresh(p string) (bool, error) {
	failed := readRefreshTime(filepath.Join(p, refreshFailureFile))
	if !failed.IsZero() && c.config.Now().Sub(failed) < RefreshInterval {
		return true, unavailable("forge", nil)
	}
	last := c.lastRefresh(p)
	return !last.IsZero() && c.config.Now().Sub(last) < RefreshInterval, nil
}

// recordRefreshResult records a forge failure, also one that ended at the
// background bound, unless the server is stopping: the next reads within
// RefreshInterval say the forge could not be reached instead of starting the
// whole transfer again.
func (c *Cache) recordRefreshResult(p string, err error) error {
	if err == nil {
		return c.recordRefresh(p)
	}
	var outage *gitport.UnavailableError
	if c.ctx.Err() == nil && errors.As(err, &outage) && outage.Op == "forge" {
		if writeErr := writeDurable(filepath.Join(p, refreshFailureFile), []byte(c.config.Now().Format(time.RFC3339Nano))); writeErr != nil {
			return writeErr
		}
	}
	return err
}

func (c *Cache) lastRefresh(p string) time.Time {
	return readRefreshTime(filepath.Join(p, "oberth-refreshed"))
}
func readRefreshTime(p string) time.Time {
	data, err := os.ReadFile(p)
	if err != nil {
		return time.Time{}
	}
	last, _ := time.Parse(time.RFC3339Nano, string(data))
	return last
}
func (c *Cache) recordRefresh(p string) error {
	if err := writeDurable(filepath.Join(p, "oberth-refreshed"), []byte(c.config.Now().Format(time.RFC3339Nano))); err != nil {
		return err
	}
	err := os.Remove(filepath.Join(p, refreshFailureFile))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// RefreshRead permits cached reads only after a forge transport failure or
// while the repository's fetch still runs. Local storage and recovery
// failures remain errors; they cannot prove a usable cache.
func (c *Cache) RefreshRead(ctx context.Context, repo string, notice io.Writer) error {
	return c.cachedRead(ctx, repo, notice, c.refresh(ctx, repo, false))
}

// ResolveFresh resolves ref where a fetch would find it. A ref the cache has
// is read after the bounded refresh. A ref the cache lacks is looked for on
// the forge with one refresh that ignores the freshness interval, at most once
// per repository per RefreshInterval (lookDue); otherwise, and when the forge
// does not answer, the cached answer stands. It refreshes quietly; the caller
// reads the result itself.
func (c *Cache) ResolveFresh(ctx context.Context, repo, ref string) (gitport.Ref, error) {
	p, err := c.path(repo)
	if err != nil {
		return gitport.Ref{}, err
	}
	cached, err := c.ResolveRef(ctx, repo, ref)
	missing := isAbsent(err)
	if missing {
		due, lookErr := c.lookDue(ctx, p)
		if lookErr != nil {
			return gitport.Ref{}, lookErr
		}
		if !due {
			return cached, err
		}
	}
	_, err = c.refreshQuietly(ctx, repo, missing)
	if err = c.cachedRead(ctx, repo, nil, err); err != nil {
		return gitport.Ref{}, err
	}
	return c.ResolveRef(ctx, repo, ref)
}

// LookForMissing asks the forge for refs the cache lacks, under the same
// lookDue bound as ResolveFresh, and returns the public refs that moved. It
// refreshes quietly, so the caller hands the moves on, and returns a forge that
// did not answer as its error.
func (c *Cache) LookForMissing(ctx context.Context, repo string) ([]RefUpdate, error) {
	p, err := c.path(repo)
	if err != nil {
		return nil, err
	}
	due, err := c.lookDue(ctx, p)
	if err != nil || !due {
		return nil, err
	}
	return c.refreshQuietly(ctx, repo, true)
}

// lookDue is the one bound on asking the forge for a ref the cache lacks: once
// per repository per RefreshInterval, whoever asks and however often. The look
// is recorded before the forge is asked, so a forge that never answers is not
// asked again within the interval either.
func (c *Cache) lookDue(ctx context.Context, p string) (bool, error) {
	slot := c.repositoryGate(p + "/look")
	if err := acquireContext(ctx, slot); err != nil {
		return false, unavailable("forge", err)
	}
	defer func() { <-slot }()
	last := readRefreshTime(filepath.Join(p, lookFile))
	if !last.IsZero() && c.config.Now().Sub(last) < RefreshInterval {
		return false, nil
	}
	return true, writeDurable(filepath.Join(p, lookFile), []byte(c.config.Now().Format(time.RFC3339Nano)))
}

// cachedRead serves the cache after a forge that did not answer, or while the
// repository's fetch still runs, and says which with the cache's age.
func (c *Cache) cachedRead(ctx context.Context, repo string, notice io.Writer, err error) error {
	var outage *gitport.UnavailableError
	var updating *gitport.FetchUnfinishedError
	forge := errors.As(err, &outage) && outage.Op == "forge"
	if !forge && !errors.As(err, &updating) || ctx.Err() != nil {
		return err
	}
	p, pathErr := c.path(repo)
	if pathErr != nil {
		return pathErr
	}
	age := "unknown"
	if last := c.lastRefresh(p); !last.IsZero() {
		age = max(time.Duration(0), c.config.Now().Sub(last)).Round(time.Second).String()
	}
	line := gitport.ForgeCacheNotice + age + "."
	reason, detail := gitport.ForgeStatusReason, repo+": "+gitport.ForgeOperatorRemedy+" "+line
	if !forge {
		line = gitport.ForgeUpdatingNotice + age + "."
		reason, detail = reasonForgeUpdating, line
	}
	if c.config.Log != nil {
		c.config.Log(reason, detail)
	}
	if notice != nil {
		_, err = fmt.Fprintln(notice, line)
		return err
	}
	return nil
}
