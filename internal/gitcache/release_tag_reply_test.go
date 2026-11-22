package gitcache_test

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/publicerr"
)

const versionUsed = "Its version is used; release the next version."

func checkDeliveredReply(t *testing.T, output string, reason string) {
	t.Helper()
	if !strings.Contains(output, "release tag delivered") || !strings.Contains(output, versionUsed) || strings.Contains(output, "no ref moved") {
		t.Fatalf("a refused push with a delivered tag did not say its version is used:\n%s", output)
	}
	if reason != "" && !strings.Contains(output, reason) {
		t.Fatalf("the callback's own reason was dropped:\n%s", output)
	}
}

// A callback that delivered the release tag and then fails, in time or past
// the deadline, refuses the push with its own reason and says that the tag is
// on the forge and its version is used, never that no ref moved.
func TestDeliveredTagRefusalSaysTheVersionIsUsed(t *testing.T) {
	t.Parallel()
	reason := "run could not be recorded"
	for _, late := range []bool{false, true} {
		t.Run(map[bool]string{false: "in time", true: "past the deadline"}[late], func(t *testing.T) {
			p := newReleaseTagPush(t)
			err := p.gate(t, time.Second, func(ctx context.Context, s *gitcache.ReceiveSource) error {
				if err := s.DeliverTag(ctx, p.tag); err != nil {
					return err
				}
				if late {
					<-ctx.Done()
				}
				return publicerr.New(publicerr.Unavailable, "The "+reason+".", "Ask the server operator.", nil)
			})
			if err == nil || p.delivered != 0 {
				t.Fatal("a failing callback after delivery was admitted", err)
			}
			checkDeliveredReply(t, p.output.String(), reason)
		})
	}
}

// A server stop that ends the wait for a callback past its delivery says the
// tag is on the forge and its version is used.
func TestServerStopAfterDeliverySaysTheVersionIsUsed(t *testing.T) {
	t.Parallel()
	p := newReleaseTagPush(t)
	f := p.f
	delivered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	lifetime, stop := context.WithCancel(context.Background())
	defer stop()
	c := configuredReceiveCache(t, f, func(cfg *gitcache.Config) {
		cfg.AdmissionTimeout = 300 * time.Millisecond
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
	var output bytes.Buffer
	r := gateReceive(f)
	r.AlwaysRun = true
	r.Output, r.Diagnostics = &output, &output
	r.Input = receiveBatchInput(t, f, []gitcache.RefUpdate{{Ref: p.tag.Name, NewSHA: p.tag.ObjectSHA}}, nil)
	r.Deliver = func(context.Context, gitcache.ReceiveEvent) ([]string, error) { return nil, nil }
	done := make(chan error, 1)
	go func() { done <- c.Receive(t.Context(), r) }()
	<-delivered
	stop()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a server stop did not end the wait for the delivering callback")
	}
	checkDeliveredReply(t, output.String(), "")
}
