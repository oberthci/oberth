package gitcache_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
)

// A callback stuck after delivering a release tag holds its receive only until
// the server lifetime the cache delivers under ends, so a stop is never held.
func TestServerStopEndsWaitingForAStuckDeliveringCallback(t *testing.T) {
	t.Parallel()
	p := newReleaseTagPush(t)
	f := p.f
	delivered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	lifetime, stop := context.WithCancel(context.Background())
	defer stop()
	c := configuredReceiveCache(t, f, func(cfg *gitcache.Config) {
		cfg.AdmissionTimeout = 500 * time.Millisecond
		cfg.DeliveryContext = lifetime
		cfg.PreReceive = func(ctx context.Context, _ gitcache.ReceiveEvent, s *gitcache.ReceiveSource) error {
			if err := s.DeliverTag(ctx, p.tag); err != nil {
				return err
			}
			close(delivered)
			<-release
			return nil
		}
		cfg.Upstream = func(context.Context, string) (gitcache.Remote, error) {
			return gitcache.Remote{RepositoryID: 1, URL: f.Bare, DefaultBranch: "refs/heads/main"}, nil
		}
	})
	r := gateReceive(f)
	r.AlwaysRun = true
	r.Input = receiveBatchInput(t, f, []gitcache.RefUpdate{{Ref: p.tag.Name, NewSHA: p.tag.ObjectSHA}}, nil)
	r.Deliver = func(context.Context, gitcache.ReceiveEvent) ([]string, error) { return nil, nil }
	done := make(chan error, 1)
	go func() { done <- c.Receive(t.Context(), r) }()
	<-delivered
	stop()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a stuck callback after delivery held the receive past the server lifetime")
	}
}
