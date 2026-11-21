package gitcache

import (
	"context"
	"errors"
	"fmt"
	"github.com/oberthci/oberth/internal/gitport"
	"io"
	"os"
	"path/filepath"
	"time"
)

// RefreshInterval bounds how long demand reads reuse a forge snapshot or failure.
const RefreshInterval = 45 * time.Second
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

func (c *Cache) refreshQuietly(ctx context.Context, repo string, force bool) ([]RefUpdate, error) {
	if c.config.Upstream == nil {
		return nil, nil
	}
	caller := ctx
	ctx, cancel := context.WithTimeout(ctx, refreshDeadline)
	defer cancel()
	p, err := c.path(repo)
	if err != nil {
		return nil, err
	}
	slot := c.repositoryGate(p + "/fetch")
	if err = acquireContext(ctx, slot); err != nil {
		return nil, unavailable("forge", err)
	}
	defer func() { <-slot }()
	if !force {
		if recent, err := c.recentRefresh(p); recent {
			return nil, err
		}
	}
	remote, err := c.remote(ctx, repo)
	if err != nil {
		return nil, err
	}
	updates, err := c.fetch(ctx, p, repo, remote)
	var refusal *RefusalError
	if !force && errors.As(err, &refusal) && refusal.Field == "pending receive" {
		// The pending receive owns the public inventory until it is settled.
		return nil, nil
	}
	return updates, c.recordRefreshResult(caller, p, err)
}

func (c *Cache) recentRefresh(p string) (bool, error) {
	failed := readRefreshTime(filepath.Join(p, refreshFailureFile))
	if !failed.IsZero() && c.config.Now().Sub(failed) < RefreshInterval {
		return true, unavailable("forge", nil)
	}
	last := c.lastRefresh(p)
	return !last.IsZero() && c.config.Now().Sub(last) < RefreshInterval, nil
}
func (c *Cache) recordRefreshResult(ctx context.Context, p string, err error) error {
	if err == nil {
		return c.recordRefresh(p)
	}
	var outage *gitport.UnavailableError
	if ctx.Err() == nil && errors.As(err, &outage) && outage.Op == "forge" {
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

// RefreshRead permits cached reads only after a forge transport failure. Local
// storage and recovery failures remain errors; they cannot prove a usable cache.
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
func (c *Cache) cachedRead(ctx context.Context, repo string, notice io.Writer, err error) error {
	var outage *gitport.UnavailableError
	if !errors.As(err, &outage) || outage.Op != "forge" || ctx.Err() != nil {
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
	if c.config.Log != nil {
		c.config.Log(gitport.ForgeStatusReason, gitport.ForgeOperatorRemedy+" "+line)
	}
	if notice != nil {
		_, err = fmt.Fprintln(notice, line)
		return err
	}
	return nil
}
