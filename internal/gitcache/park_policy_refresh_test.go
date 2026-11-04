package gitcache

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

type slowPushPolicyUpstream struct{ gitBackend }

func (b slowPushPolicyUpstream) execute(ctx context.Context, spec commandSpec, capture bool) (string, error) {
	if len(spec.args) > 0 && spec.args[0] == "fetch" {
		timer := time.NewTimer(2100 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-timer.C:
		}
	}
	return b.gitBackend.execute(ctx, spec, capture)
}

func TestPushPolicySurvivesUpstreamSlowerThanReadDeadline(t *testing.T) {
	r := newTestRepository(t)
	sha := r.commitFile(t, "policy", "policy.yaml", "required\n")
	c := newTestCache(t, r.upstream)
	c.backend = slowPushPolicyUpstream{c.backend}
	if _, err := c.Ensure(WithBurstPolicy(t.Context()), "example"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(WithCachedPolicy(t.Context()), 2*time.Second)
	defer cancel()
	actual, body, exists, err := c.ReadDefaultBlob(ctx, "example", "policy.yaml", 4096)
	if err != nil || actual != sha || !exists || string(body) != "required\n" {
		t.Fatalf("slow successful upstream caused fallback: %s %q %v %v", actual, body, exists, err)
	}
}

// Push admission already refreshes upstream in Ensure. A second fetch can
// exhaust its two-second policy budget even though the first one succeeded.
func TestPushPolicyReusesSuccessfulEnsureRefresh(t *testing.T) {
	r := newTestRepository(t)
	sha := r.commitFile(t, "policy", "policy.yaml", "required\n")
	c := newTestCache(t, r.upstream)
	if _, err := c.Ensure(t.Context(), "example"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Ensure(WithBurstPolicy(t.Context()), "example"); err != nil {
		t.Fatal(err)
	}
	// All local Git plumbing remains real. Only an unnecessary upstream fetch
	// stalls, reproducing the admission deadline without a network dependency.
	c.backend = stalledPolicyFetch{c.backend}
	ctx, cancel := context.WithTimeout(WithBurstPolicy(t.Context()), 2*time.Second)
	defer cancel()
	actual, body, exists, err := c.ReadDefaultBlob(ctx, "example", "policy.yaml", 4096)
	if err != nil || actual != sha || !exists || string(body) != "required\n" {
		t.Fatalf("successful upstream refresh lost at push admission: sha=%s body=%q exists=%v err=%v", actual, body, exists, err)
	}
}

func TestPushPolicyRefreshReusePreservesAuthority(t *testing.T) {
	for _, scenario := range []string{"fresh reader", "expired", "changed remote", "changed tracking ref", "failed ensure"} {
		t.Run(scenario, func(t *testing.T) {
			r := newTestRepository(t)
			r.commitFile(t, "policy", "policy.yaml", "required\n")
			c := newTestCache(t, r.upstream)
			if _, err := c.Ensure(t.Context(), "example"); err != nil {
				t.Fatal(err)
			}
			ready, err := c.Ensure(WithBurstPolicy(t.Context()), "example")
			if err != nil {
				t.Fatal(err)
			}
			ctx := WithBurstPolicy(t.Context())
			switch scenario {
			case "fresh reader":
				ctx = WithFreshPolicy(ctx)
			case "expired":
				value, ok := c.policyRefreshes.Load(ready.Path)
				if !ok {
					t.Fatal("successful push refresh was not retained")
				}
				entry := value.(policyRefresh)
				entry.completed = time.Now().Add(-policyRefreshTTL)
				c.policyRefreshes.Store(ready.Path, entry)
			case "changed remote":
				other := newTestRepository(t)
				c.upstream = func(string) (string, error) { return other.upstream, nil }
			case "changed tracking ref":
				runGit(t, ready.Path, "update-ref", upstreamRefPrefix+"heads/main", r.initialSHA)
			}
			c.backend = stalledPolicyFetch{c.backend}
			if scenario == "failed ensure" {
				refreshCtx, cancel := context.WithTimeout(WithBurstPolicy(t.Context()), 100*time.Millisecond)
				stale, refreshErr := c.Ensure(refreshCtx, "example")
				cancel()
				// A caller cancellation can also stop the stale-cache plumbing.
				if refreshErr == nil && !stale.Stale {
					t.Fatal("failed refresh reported fresh upstream state")
				}
			}
			ctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
			defer cancel()
			if _, _, _, err := c.ReadDefaultBlob(ctx, "example", "policy.yaml", 4096); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("%s reused stale authority: %v", scenario, err)
			}
		})
	}
}

func TestPushPolicyRefreshReuseIgnoresPublicDefaultBranch(t *testing.T) {
	r := newTestRepository(t)
	sha := r.commitFile(t, "policy", "policy.yaml", "required\n")
	c := newTestCache(t, r.upstream)
	ready, err := c.Ensure(t.Context(), "example")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Ensure(WithBurstPolicy(t.Context()), "example"); err != nil {
		t.Fatal(err)
	}
	// Public refs can move during receive. They must never supply policy.
	runGit(t, ready.Path, "update-ref", "refs/heads/main", r.initialSHA)
	c.backend = stalledPolicyFetch{c.backend}
	ctx, cancel := context.WithTimeout(WithBurstPolicy(t.Context()), 2*time.Second)
	defer cancel()
	actual, body, exists, err := c.ReadDefaultBlob(ctx, "example", "policy.yaml", 4096)
	if err != nil || actual != sha || !exists || string(body) != "required\n" {
		t.Fatalf("public default replaced upstream authority: %s %q %v %v", actual, body, exists, err)
	}
}

func TestCachedPushPolicyNeverOpensTransport(t *testing.T) {
	for _, scenario := range []string{"initial ensure", "missing policy", "unborn", "no snapshot", "expired", "fresh override"} {
		t.Run(scenario, func(t *testing.T) {
			r := newTestRepository(t)
			upstream := r.upstream
			if scenario == "unborn" {
				upstream = filepath.Join(t.TempDir(), "empty.git")
				runGit(t, "", "init", "--bare", "--initial-branch=main", upstream)
			}
			c := newTestCache(t, upstream)
			ctx := WithBurstPolicy(t.Context())
			if scenario == "no snapshot" {
				ctx = t.Context()
			}
			ready, err := c.Ensure(ctx, "example")
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "expired" {
				value, ok := c.policyRefreshes.Load(ready.Path)
				if !ok {
					t.Fatal("successful refresh missing")
				}
				entry := value.(policyRefresh)
				entry.completed = time.Now().Add(-policyRefreshTTL)
				c.policyRefreshes.Store(ready.Path, entry)
			}
			backend := &refusingPolicyUpstream{gitBackend: c.backend, refusals: 100, ceiling: 1}
			c.backend = backend
			ctx, cancel := context.WithTimeout(WithCachedPolicy(t.Context()), 2*time.Second)
			defer cancel()
			if scenario == "fresh override" {
				ctx = WithFreshPolicy(ctx)
			}
			sha, _, exists, err := c.ReadDefaultBlob(ctx, "example", "absent.yaml", 4096)
			switch scenario {
			case "no snapshot", "expired":
				if !errors.Is(err, ErrPolicySnapshotUnavailable) {
					t.Fatalf("unavailable snapshot: %v", err)
				}
			case "fresh override":
				if err == nil || backend.attempts == 0 {
					t.Fatalf("fresh policy reused push snapshot: attempts=%d err=%v", backend.attempts, err)
				}
				return
			default:
				if err != nil || exists || (scenario != "unborn" && sha != r.initialSHA) || (scenario == "unborn" && sha != "") {
					t.Fatalf("absent upstream policy: sha=%s exists=%v err=%v", sha, exists, err)
				}
			}
			if backend.attempts != 0 {
				t.Fatalf("cached push policy opened %d transports", backend.attempts)
			}
		})
	}
}
