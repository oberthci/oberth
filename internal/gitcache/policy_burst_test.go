package gitcache

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// refusingPolicyUpstream models a Git host with a connection ceiling and an
// initial transport outage. Local plumbing still goes through real Git.
type refusingPolicyUpstream struct {
	gitBackend
	mu                                  sync.Mutex
	refusals, attempts, active, ceiling int
}

func TestPolicyRefreshCacheAuthorityAndExpiry(t *testing.T) {
	repository := newTestRepository(t)
	first := repository.commitFile(t, "first policy", "policy.yaml", "first\n")
	cache := newTestCache(t, repository.upstream)
	ctx := t.Context()
	cached, err := cache.Ensure(ctx, "example")
	if err != nil {
		t.Fatal(err)
	}
	read := func(ctx context.Context, want string) {
		t.Helper()
		sha, _, _, err := cache.ReadDefaultBlob(ctx, "example", "policy.yaml", 4096)
		if err != nil || sha != want {
			t.Fatalf("sha = %s, want %s; err=%v", sha, want, err)
		}
	}
	read(ctx, first)
	second := repository.commitFile(t, "second policy", "policy.yaml", "second\n")
	read(ctx, first)                   // Explicitly bounded burst reuse.
	read(WithFreshPolicy(ctx), second) // Publication sees new authority now.
	third := repository.commitFile(t, "third policy", "policy.yaml", "third\n")
	value, _ := cache.policyRefreshes.Load(cached.Path)
	entry := value.(policyRefresh)
	entry.completed = time.Now().Add(-policyRefreshTTL)
	cache.policyRefreshes.Store(cached.Path, entry)
	read(ctx, third)
	// A different tracking commit invalidates a recent entry, even within TTL.
	runGit(t, cached.Path, "update-ref", upstreamRefPrefix+"heads/main", first)
	read(ctx, third)
	// Changing the upstream identity cannot reuse another remote's authority.
	other := newTestRepository(t)
	fourth := other.commitFile(t, "other policy", "policy.yaml", "other\n")
	cache.upstream = func(string) (string, error) { return other.upstream, nil }
	read(ctx, fourth)
	// A forced refresh failure invalidates prior success for ordinary readers.
	upstream := &refusingPolicyUpstream{gitBackend: cache.backend, refusals: 100, ceiling: 1}
	cache.backend = upstream
	if _, _, _, err := cache.ReadDefaultBlob(WithFreshPolicy(ctx), "example", "policy.yaml", 4096); err == nil {
		t.Fatal("failed fresh read reused policy")
	}
	if _, _, _, err := cache.ReadDefaultBlob(ctx, "example", "policy.yaml", 4096); err == nil {
		t.Fatal("burst read resurrected stale policy")
	}
	if upstream.attempts != 4 {
		t.Fatalf("failure cooldown opened %d connections", upstream.attempts)
	}
}

func TestPolicyRefreshTransportClassification(t *testing.T) {
	for _, test := range []struct {
		message   string
		transient bool
	}{
		{"oberth git-ssh: transport refused or upstream unavailable", true},
		{"connection reset by peer", true},
		{"git command failed: exit status 128", false},
		{"oberth git-ssh: transport refused or upstream: permission denied", false},
		{"repository not found", false},
	} {
		if got := transientPolicyTransport(errors.New(test.message)); got != test.transient {
			t.Errorf("%q: transient=%v", test.message, got)
		}
	}
	if transientPolicyTransport(context.Canceled) {
		t.Fatal("cancellation is retryable")
	}
}

func (b *refusingPolicyUpstream) execute(ctx context.Context, spec commandSpec, capture bool) (string, error) {
	if len(spec.args) == 0 || (spec.args[0] != "fetch" && spec.args[0] != "ls-remote") {
		return b.gitBackend.execute(ctx, spec, capture)
	}
	b.mu.Lock()
	b.attempts++
	b.active++
	refuse := b.active > b.ceiling || b.refusals > 0
	if b.refusals > 0 {
		b.refusals--
	}
	b.mu.Unlock()
	defer func() { b.mu.Lock(); b.active--; b.mu.Unlock() }()
	if refuse {
		return "", errors.New("git command failed: exit status 128: oberth git-ssh: transport refused or upstream unavailable")
	}
	return b.gitBackend.execute(ctx, spec, capture)
}

type policyRetryLog struct{ lines []string }

func (l *policyRetryLog) Printf(format string, args ...any) {
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func TestPolicyRefreshBurstSharesRetries(t *testing.T) {
	for _, refusals := range []int{0, 2, 100} {
		t.Run(fmt.Sprint(refusals), func(t *testing.T) {
			repository := newTestRepository(t)
			sha := repository.commitFile(t, "policy", "policy.yaml", "required\n")
			cache := newTestCache(t, repository.upstream)
			if _, err := cache.Ensure(t.Context(), "example"); err != nil {
				t.Fatal(err)
			}
			upstream := &refusingPolicyUpstream{gitBackend: cache.backend, refusals: refusals, ceiling: 1}
			cache.backend = upstream
			logger := &policyRetryLog{}
			cache.logger = logger
			const runs = 12
			start := make(chan struct{})
			results := make(chan error, runs)
			for range runs {
				go func() {
					<-start
					actual, body, exists, err := cache.ReadDefaultBlob(t.Context(), "example", "policy.yaml", 4096)
					if refusals < 100 && err != nil {
						results <- fmt.Errorf("read policy: %w", err)
						return
					}
					if refusals < 100 && (actual != sha || !exists || string(body) != "required\n") {
						results <- fmt.Errorf("policy = %s %q %v", actual, body, exists)
						return
					}
					if refusals == 100 && err == nil {
						results <- errors.New("outage used stale policy")
						return
					}
					results <- nil
				}()
			}
			close(start)
			for range runs {
				if err := <-results; err != nil {
					t.Error(err)
				}
			}
			want := 2 + refusals // fetch and default-branch advertisement
			if refusals == 100 {
				want = 4
			}
			if upstream.attempts != want {
				t.Errorf("upstream connections = %d, want %d for the whole burst", upstream.attempts, want)
			}
			retries := 0
			for _, line := range logger.lines {
				if strings.Contains(line, "policy refresh retry") {
					retries++
				}
			}
			wantRetries := min(refusals, 3)
			if retries != wantRetries {
				t.Errorf("retry logs = %d, want %d", retries, wantRetries)
			}
		})
	}
}
