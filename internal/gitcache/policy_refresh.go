package gitcache

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/gitssh"
	"github.com/oberthci/oberth/internal/pushretry"
)

const policyRefreshTTL = 2 * time.Second
const policyFailureTTL = 5 * time.Second
const policyRefreshAttempts = 4
const policyReadTimeout = 30 * time.Second

type burstPolicyKey struct{}

// WithBurstPolicy opts initial admission into bounded successful snapshot reuse.
// Other policy readers require a fetch begun after their request by default.
func WithBurstPolicy(ctx context.Context) context.Context {
	return context.WithValue(ctx, burstPolicyKey{}, true)
}

// WithFreshPolicy overrides any inherited burst reuse. Only a refresh started
// after this reader requested authority may satisfy the read.
func WithFreshPolicy(ctx context.Context) context.Context {
	return context.WithValue(ctx, burstPolicyKey{}, false)
}

type policyRefresh struct {
	remote, ref, sha   string
	started, completed time.Time
	err                error
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
	if value, ok := c.policyRefreshes.Load(path); ok {
		previous := value.(policyRefresh)
		ttl := policyRefreshTTL
		if previous.err != nil {
			ttl = policyFailureTTL
		}
		burst, _ := ctx.Value(burstPolicyKey{}).(bool)
		if previous.remote == remote && time.Since(previous.completed) < ttl && (burst || previous.err != nil || previous.started.After(requested)) {
			if previous.err != nil {
				return "", previous.err
			}
			sha, readErr := c.capture(ctx, path, "rev-parse", "--verify", previous.ref+"^{commit}")
			if readErr == nil && strings.TrimSpace(sha) == previous.sha {
				return previous.sha, nil
			}
		}
	}
	// Invalidate first: an unsuccessful refresh cannot resurrect old authority.
	c.policyRefreshes.Delete(path)
	result := policyRefresh{remote: remote, started: time.Now()}
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
	result.completed = time.Now()
	// Unborn repositories must be rechecked, and caller cancellation must not
	// poison followers whose independent contexts are still alive.
	if ctx.Err() == nil && (result.sha != "" || transientPolicyTransport(result.err)) {
		c.policyRefreshes.Store(path, result)
	}
	return result.sha, result.err
}

func transientPolicyTransport(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	// Explicit authentication/policy errors take precedence over ambiguous
	// transport-wrapper wording. Exit status 128 alone is never retryable.
	message := strings.ToLower(err.Error())
	for _, permanent := range []string{gitssh.RefusalMessage, "oberth git-ssh: transport refused or upstream", "permission denied", "authentication failed", "access denied", "not authorized", "not allowed", "repository not found"} {
		if strings.Contains(message, permanent) {
			return false
		}
	}
	return strings.Contains(message, gitssh.TemporaryFailureMessage) || pushretry.Transient(err)
}
