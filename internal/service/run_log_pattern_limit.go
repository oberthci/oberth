package service

import (
	"context"
	"sync"

	"github.com/oberthci/oberth/internal/api"
)

type runLogCallerKey struct{}

// Use the authenticated credential, not the editable display identity. Legacy
// authenticators without a credential ID share a fingerprint bucket; unbound
// internal actors conservatively share one bucket. Nothing secret is retained.
func withRunLogCaller(ctx context.Context, actor api.Actor) context.Context {
	key := "unbound"
	if actor.TokenCredentialID != "" {
		key = "credential:" + actor.TokenCredentialID
	} else if actor.Fingerprint != "" {
		key = "fingerprint:" + actor.Fingerprint
	}
	return context.WithValue(ctx, runLogCallerKey{}, key)
}

type patternScanLimiter struct {
	global chan struct{}
	mu     sync.Mutex
	owners map[string]chan struct{}
}

// Acquire the token before the global slot: extra requests from one token must
// not reserve both global slots while waiting for their own turn. Only active
// or globally queued owners occupy the map; canceled/completed owners remove
// their entries and wake token waiters. The caller's existing read budget
// bounds both waits. One token may have at most one scan in the backend.
func (limiter *patternScanLimiter) acquire(ctx context.Context) (func(), error) {
	key, _ := ctx.Value(runLogCallerKey{}).(string)
	if key == "" {
		key = "unbound"
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		limiter.mu.Lock()
		if wait := limiter.owners[key]; wait != nil {
			limiter.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-wait:
			}
			continue
		}
		if limiter.owners == nil {
			limiter.owners = make(map[string]chan struct{})
		}
		done := make(chan struct{})
		limiter.owners[key] = done
		limiter.mu.Unlock()
		releaseToken := func() {
			limiter.mu.Lock()
			delete(limiter.owners, key)
			close(done)
			limiter.mu.Unlock()
		}
		select {
		case limiter.global <- struct{}{}:
			release := sync.OnceFunc(func() { <-limiter.global; releaseToken() })
			if err := ctx.Err(); err != nil {
				release()
				return nil, err
			}
			return release, nil
		case <-ctx.Done():
			releaseToken()
			return nil, ctx.Err()
		}
	}
}
