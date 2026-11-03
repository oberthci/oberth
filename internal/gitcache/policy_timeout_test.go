package gitcache

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestPolicyReadTimeoutStartsAfterRepositoryLock(t *testing.T) {
	r := newTestRepository(t)
	want := r.commitFile(t, "policy", "policy.yaml", "required\n")
	c := newTestCache(t, r.upstream)
	cached, err := c.Ensure(t.Context(), "example")
	if err != nil {
		t.Fatal(err)
	}
	lock := c.repoLock(cached.Path)
	lock.Lock()
	locked := true
	defer func() {
		if locked {
			lock.Unlock()
		}
	}()
	done := make(chan error, 1)
	go func() {
		sha, _, exists, err := c.ReadDefaultBlob(t.Context(), "example", "policy.yaml", 4096)
		if err == nil && (sha != want || !exists) {
			err = errors.New("missing fresh policy")
		}
		done <- err
	}()
	// Exercise the production limit, without adding a timeout seam that would
	// prevent this regression from compiling on the previous implementation.
	timer := time.NewTimer(policyReadTimeout + time.Second)
	defer timer.Stop()
	select {
	case err := <-done:
		t.Fatalf("queued reader exhausted its read budget before acquiring the lock: %v", err)
	case <-timer.C:
	}
	lock.Unlock()
	locked = false
	if err := <-done; err != nil {
		t.Fatalf("queued reader failed after lock release: %v", err)
	}
}

type stalledPolicyFetch struct{ gitBackend }

func (b stalledPolicyFetch) execute(ctx context.Context, spec commandSpec, capture bool) (string, error) {
	if len(spec.args) > 0 && spec.args[0] == "fetch" {
		<-ctx.Done()
		return "", ctx.Err()
	}
	return b.gitBackend.execute(ctx, spec, capture)
}

func TestPolicyInternalTimeoutIsUnavailableButCallerDeadlineIsNot(t *testing.T) {
	for _, callerDeadline := range []bool{false, true} {
		t.Run(map[bool]string{false: "internal", true: "caller"}[callerDeadline], func(t *testing.T) {
			r := newTestRepository(t)
			r.commitFile(t, "policy", "policy.yaml", "required\n")
			c := newTestCache(t, r.upstream)
			if _, err := c.Ensure(t.Context(), "example"); err != nil {
				t.Fatal(err)
			}
			c.backend = stalledPolicyFetch{c.backend}
			ctx := t.Context()
			if callerDeadline {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 100*time.Millisecond)
				defer cancel()
			}
			_, _, _, err := c.ReadDefaultBlob(ctx, "example", "policy.yaml", 4096)
			if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrPolicyUpstreamUnavailable) == callerDeadline {
				t.Fatalf("wrong timeout classification: %v", err)
			}
		})
	}
}
