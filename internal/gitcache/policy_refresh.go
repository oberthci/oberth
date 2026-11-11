package gitcache

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/pushretry"
)

const policyRefreshTTL = 2 * time.Second
const policyFailureTTL = 5 * time.Second
const policyRefreshAttempts = 4
const policyReadTimeout = 30 * time.Second

type burstPolicyKey struct{}
type cachedPolicyKey struct{}

// policyNow controls snapshot freshness, independently of IO deadlines and retry delays.
func (c *Cache) policyNow() time.Time {
	if c.policyClock != nil {
		return c.policyClock()
	}
	return time.Now()
}

// WithBurstPolicy opts initial admission into bounded successful snapshot reuse.
// Other policy readers require a fetch begun after their request by default.
func WithBurstPolicy(ctx context.Context) context.Context {
	return context.WithValue(context.WithValue(ctx, burstPolicyKey{}, true), cachedPolicyKey{}, false)
}

// WithCachedPolicy permits only local reads of a recent successful upstream
// snapshot. Push admission has already refreshed upstream in Ensure; it must
// not start another transport under its short policy-read deadline.
func WithCachedPolicy(ctx context.Context) context.Context {
	return context.WithValue(WithBurstPolicy(ctx), cachedPolicyKey{}, true)
}

// WithFreshPolicy overrides any inherited burst reuse. Only a refresh started
// after this reader requested authority may satisfy the read.
func WithFreshPolicy(ctx context.Context) context.Context {
	return context.WithValue(context.WithValue(ctx, burstPolicyKey{}, false), cachedPolicyKey{}, false)
}

// rememberPushPolicyLocked retains only a successful upstream refresh, never
// public refs or a decoded authorization decision. The ordinary policy reader
// still validates the remote identity, private ref, age and regular blob.
func (c *Cache) rememberPushPolicyLocked(ctx context.Context, path, branch string, started time.Time) {
	burst, _ := ctx.Value(burstPolicyKey{}).(bool)
	if !burst {
		return
	}
	remote, err := c.capture(ctx, path, "remote", "get-url", "upstream")
	if err != nil {
		return
	}
	ref := upstreamRefPrefix + "heads/" + branch
	sha, err := c.capture(ctx, path, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		absent, readErr := c.trackingBranchAbsent(ctx, path, branch)
		if readErr != nil || !absent {
			return // Unreadable default state must not become an absent policy.
		}
		sha = "" // The successful fetch proved an unborn default branch.
	} else if ValidateSHA(strings.TrimSpace(sha)) != nil {
		return
	}
	c.policyRefreshes.Store(path, policyRefresh{remote: remote, ref: ref, sha: strings.TrimSpace(sha), started: started, completed: c.policyNow()})
}

type policyRefresh struct {
	remote, ref, sha   string
	started, completed time.Time
	err                error
	// cachedOnly marks a failed Ensure refresh recorded only so cached
	// readers report its cause; fresh readers still fetch.
	cachedOnly bool
}

// rememberFailedRefreshLocked records why Ensure's refresh failed, for cached
// policy readers within the failure cooldown, in place of a bare "snapshot
// unavailable".
func (c *Cache) rememberFailedRefreshLocked(path, remote string, started time.Time, err error) {
	if remote == "" || err == nil {
		return
	}
	err = fmt.Errorf("refresh upstream: %w", err)
	if transientPolicyTransport(err) {
		err = fmt.Errorf("%w: %w", ErrPolicyUpstreamUnavailable, err)
	}
	c.policyRefreshes.Store(path, policyRefresh{remote: remote, started: started, completed: c.policyNow(), err: err, cachedOnly: true})
}

// The repository lock is the flight gate: queued readers reuse its result,
// including a short failure cooldown, rather than opening their own transports.
// Cache authority includes the upstream URL, private ref and fetched commit;
// no candidate content or decoded authorization decision is cached.
func (c *Cache) refreshPolicyLocked(ctx context.Context, input, path string, requested time.Time) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	// configureRemote already validated this URL under the same repository
	// lock. Key the actual configured transport, not a second catalog lookup
	// that could observe an upstream edit between configuration and fetch.
	remote, err := c.capture(ctx, path, "remote", "get-url", "upstream")
	if err != nil {
		return "", err
	}
	cachedOnly, _ := ctx.Value(cachedPolicyKey{}).(bool)
	if value, ok := c.policyRefreshes.Load(path); ok && (cachedOnly || !value.(policyRefresh).cachedOnly) {
		previous := value.(policyRefresh)
		ttl := policyRefreshTTL
		if previous.err != nil {
			ttl = policyFailureTTL
		}
		burst, _ := ctx.Value(burstPolicyKey{}).(bool)
		if previous.remote == remote && c.policyNow().Sub(previous.completed) < ttl && (burst || previous.err != nil || previous.started.After(requested)) {
			if previous.err != nil {
				return "", previous.err
			}
			if previous.sha == "" {
				if absent, readErr := c.trackingBranchAbsent(ctx, path, strings.TrimPrefix(previous.ref, upstreamRefPrefix+"heads/")); readErr == nil && absent {
					return "", nil
				}
			} else {
				sha, readErr := c.capture(ctx, path, "rev-parse", "--verify", previous.ref+"^{commit}")
				if readErr == nil && strings.TrimSpace(sha) == previous.sha {
					return previous.sha, nil
				}
			}
		}
	}
	if cachedOnly {
		return "", ErrPolicySnapshotUnavailable
	}
	// Invalidate first: an unsuccessful refresh cannot resurrect old authority.
	c.policyRefreshes.Delete(path)
	result := policyRefresh{remote: remote, started: c.policyNow()}
	for attempt := 1; attempt <= policyRefreshAttempts; attempt++ {
		result.ref, result.sha, result.err = c.fetchPolicyLocked(ctx, path)
		if result.err == nil || !transientPolicyTransport(result.err) || ctx.Err() != nil || attempt == policyRefreshAttempts {
			break
		}
		// Equal jitter keeps exponential spacing while preventing lockstep retries
		// across repositories. Four attempts wait at most 3.5 seconds in total.
		ceiling := (500 * time.Millisecond) << (attempt - 1)
		delay := ceiling/2 + time.Duration(rand.Int64N(int64(ceiling/2))) //nolint:gosec // Public scheduling jitter, not a security value.
		c.logger.Printf("policy refresh retry: repository=%q attempt=%d max_attempts=%d delay=%s", input, attempt+1, policyRefreshAttempts, delay)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", ctx.Err()
		case <-timer.C:
		}
	}
	if transientPolicyTransport(result.err) {
		result.err = fmt.Errorf("%w: %w", ErrPolicyUpstreamUnavailable, result.err)
	}
	result.completed = c.policyNow()
	// Unborn repositories must be rechecked, and caller cancellation must not
	// poison followers whose independent contexts are still alive.
	if ctx.Err() == nil && (result.sha != "" || transientPolicyTransport(result.err)) {
		c.policyRefreshes.Store(path, result)
	}
	return result.sha, result.err
}

// transientPolicyTransport shares the publication classifier, which knows
// the git-ssh shim's own transport and refusal lines. A result already
// classified as an upstream outage stays one.
func transientPolicyTransport(err error) bool {
	if errors.Is(err, ErrPolicyUpstreamUnavailable) {
		return true
	}
	return pushretry.Transient(err)
}
