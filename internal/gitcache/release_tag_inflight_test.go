package gitcache_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/publicerr"
)

// holdForgeTagPush lets the forge push of tag land, then holds the push
// command until it is killed, the way a forge write is still under way when
// the admission deadline fires.
func holdForgeTagPush(t *testing.T, binary, tag string) string {
	t.Helper()
	dir := t.TempDir()
	gate, script := filepath.Join(dir, "gate"), filepath.Join(dir, "git")
	if err := syscall.Mkfifo(gate, 0600); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`#!/bin/sh
push= tag=
for a do
  [ "$a" = push ] && push=1
  case "$a" in *'%s'*) tag=1;; esac
done
if [ -n "$push" ] && [ -n "$tag" ]; then
  '%s' "$@"
  read _ < '%s'
  exit 1
fi
exec '%s' "$@"
`, tag, binary, gate, binary)
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	return script
}

// pushReleaseTag runs one push of p's release tag whose admission delivers it
// as the composition does, refusing with the delivery's own words.
func pushReleaseTag(t *testing.T, p *releaseTagPush, configure func(*gitcache.Config), admit func(context.Context, *gitcache.ReceiveSource) error) (string, error) {
	t.Helper()
	c, r, output := releaseTagReceive(t, p, configure, admit)
	err := c.Receive(t.Context(), r)
	return output.String(), err
}

// releaseTagReceive builds the cache and the receive of p's release tag.
func releaseTagReceive(t *testing.T, p *releaseTagPush, configure func(*gitcache.Config), admit func(context.Context, *gitcache.ReceiveSource) error) (*gitcache.Cache, gitcache.ReceiveRequest, *bytes.Buffer) {
	t.Helper()
	f := p.f
	c := configuredReceiveCache(t, f, func(cfg *gitcache.Config) {
		cfg.AdmissionTimeout = time.Second
		cfg.PreReceive = func(ctx context.Context, _ gitcache.ReceiveEvent, s *gitcache.ReceiveSource) error {
			return admit(ctx, s)
		}
		cfg.Upstream = func(context.Context, string) (gitcache.Remote, error) {
			return gitcache.Remote{RepositoryID: 1, URL: f.Bare, DefaultBranch: "refs/heads/main"}, nil
		}
		configure(cfg)
	})
	output := &bytes.Buffer{}
	r := gateReceive(f)
	r.Output, r.Diagnostics = output, output
	r.AlwaysRun = true
	r.Input = receiveBatchInput(t, f, []gitcache.RefUpdate{{Ref: p.tag.Name, NewSHA: p.tag.ObjectSHA}}, nil)
	r.Deliver = func(context.Context, gitcache.ReceiveEvent) ([]string, error) {
		p.delivered++
		return nil, nil
	}
	return c, r, output
}

func deliverAsComposed(ctx context.Context, s *gitcache.ReceiveSource, p *releaseTagPush) error {
	if err := s.DeliverTag(ctx, p.tag); err != nil {
		return publicerr.New(publicerr.Unavailable, "release tag not delivered to the forge", "Check the forge: if the tag is there, this version is used; release the next one.", err)
	}
	return nil
}

// A forge write of the release tag under way when the admission deadline
// fires is waited for: the pusher reads the delivery's own outcome (here:
// check the forge, the version may be used), never "no ref moved" while the
// tag is on the forge.
func TestReleaseTagWriteInFlightAtTheDeadlineIsWaitedFor(t *testing.T) {
	t.Parallel()
	p := newReleaseTagPush(t)
	script := holdForgeTagPush(t, p.f.Binary, p.tag.Name)
	output, err := pushReleaseTag(t, p, func(cfg *gitcache.Config) { cfg.Binary = script }, func(ctx context.Context, s *gitcache.ReceiveSource) error {
		return deliverAsComposed(ctx, s, p)
	})
	if p.forgeTag() != p.tag.ObjectSHA {
		t.Fatal("the held forge push did not land", output)
	}
	if strings.Contains(output, "no ref moved") {
		t.Fatalf("a push with its tag on the forge was told no ref moved:\n%s", output)
	}
	if err != nil && !strings.Contains(output, "this version is used") && !strings.Contains(output, "Its version is used") {
		t.Fatalf("a refused push with its tag on the forge did not say the version may be used: %v\n%s", err, output)
	}
}

// A server stop that ends the wait for a callback whose delivery began but
// was never recorded gives the neutral reply: it claims neither that no ref
// moved nor that the version is used.
func TestServerStopBeforeADeliveryIsRecordedIsNeutral(t *testing.T) {
	t.Parallel()
	p := newReleaseTagPush(t)
	if err := p.toForge("HEAD"); err != nil {
		t.Fatal(err)
	}
	lifetime, stop := context.WithCancel(context.Background())
	defer stop()
	release := make(chan struct{})
	defer close(release)
	attempted := make(chan error, 1)
	c, r, output := releaseTagReceive(t, p, func(cfg *gitcache.Config) {
		cfg.AdmissionTimeout = 300 * time.Millisecond
		cfg.DeliveryContext = lifetime
	}, func(ctx context.Context, s *gitcache.ReceiveSource) error {
		err := s.DeliverTag(ctx, p.tag)
		attempted <- err
		<-release
		return err
	})
	done := make(chan error, 1)
	go func() { done <- c.Receive(t.Context(), r) }()
	if err := <-attempted; err == nil {
		t.Fatal("a tag held at another object on the forge was delivered")
	}
	select {
	case err := <-done:
		t.Fatal("the gate did not wait for a callback whose delivery began", err)
	case <-time.After(time.Second):
	}
	stop()
	if err := <-done; err == nil || !strings.Contains(output.String(), "push admission is unavailable") || strings.Contains(output.String(), "no ref moved") || strings.Contains(output.String(), "version is used") {
		t.Fatalf("a stop before a recorded delivery was not answered neutrally: %v\n%s", err, output.String())
	}
}

// A delivery started after the admission ended writes nothing to the forge.
func TestReleaseTagDeliveryAfterTheDeadlineWritesNothing(t *testing.T) {
	t.Parallel()
	p := newReleaseTagPush(t)
	deliveries := make(chan error, 1)
	_, err := pushReleaseTag(t, p, func(*gitcache.Config) {}, func(ctx context.Context, s *gitcache.ReceiveSource) error {
		<-ctx.Done()
		delivery := s.DeliverTag(ctx, p.tag)
		deliveries <- delivery
		return delivery
	})
	delivery := <-deliveries
	if err == nil || delivery == nil || p.forgeTag() != "" || p.delivered != 0 {
		t.Fatal("a delivery after the deadline reached the forge", err, delivery, p.forgeTag())
	}
}
