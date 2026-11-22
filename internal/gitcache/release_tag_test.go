package gitcache_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/gitport"
)

// releaseTagPush pushes a new commit and an annotated tag on it to the cache.
// Its gate delivers the tag; the delivery callback, where the run is created,
// records what the forge held at that moment.
type releaseTagPush struct {
	f         *gittest.Fixture
	tag       gitport.Ref
	forgeThen string
	delivered int
	output    bytes.Buffer
}

func newReleaseTagPush(t *testing.T) *releaseTagPush {
	t.Helper()
	f := gittest.New(t)
	f.Write(t, "file.txt", []byte("released source\n"))
	commit := f.CommitFile(t, "release")
	f.Git(t, f.Work, "tag", "-a", "v1.2.3", "-m", "v1.2.3", commit)
	tag := f.Git(t, f.Work, "rev-parse", "refs/tags/v1.2.3")
	return &releaseTagPush{f: f, tag: gitport.Ref{Name: "refs/tags/v1.2.3", ObjectSHA: tag, CommitSHA: commit}}
}

// receive runs one push whose gate first lets the forge gain forgeFirst (a
// local tag, after the cache last read the forge) and then delivers ref.
func (p *releaseTagPush) receive(t *testing.T, forgeFirst string, ref gitport.Ref) (delivery, receive error) {
	t.Helper()
	receive = p.gate(t, 0, func(ctx context.Context, s *gitcache.ReceiveSource) error {
		if forgeFirst != "" {
			if err := p.toForge(forgeFirst); err != nil {
				return err
			}
		}
		delivery = s.DeliverTag(ctx, ref)
		return delivery
	})
	return delivery, receive
}

// gate runs one push of the tag with the given admission callback and timeout.
func (p *releaseTagPush) gate(t *testing.T, timeout time.Duration, admit func(context.Context, *gitcache.ReceiveSource) error) error {
	t.Helper()
	f := p.f
	c := configuredReceiveCache(t, f, func(cfg *gitcache.Config) {
		cfg.AdmissionTimeout = timeout
		cfg.PreReceive = func(ctx context.Context, _ gitcache.ReceiveEvent, s *gitcache.ReceiveSource) error {
			return admit(ctx, s)
		}
		cfg.Upstream = func(context.Context, string) (gitcache.Remote, error) {
			return gitcache.Remote{RepositoryID: 1, URL: f.Bare, DefaultBranch: "refs/heads/main"}, nil
		}
	})
	r := gateReceive(f)
	r.Output, r.Diagnostics = &p.output, &p.output
	r.AlwaysRun = true
	r.Input = receiveBatchInput(t, f, []gitcache.RefUpdate{{Ref: p.tag.Name, NewSHA: p.tag.ObjectSHA}}, nil)
	r.Deliver = func(context.Context, gitcache.ReceiveEvent) ([]string, error) {
		p.delivered++
		p.forgeThen = p.forgeTag()
		return nil, nil
	}
	return c.Receive(t.Context(), r)
}

func (p *releaseTagPush) toForge(local string) error {
	out, err := exec.Command(p.f.Binary, "-C", p.f.Work, "push", "--quiet", "--", p.f.Bare, local+":"+p.tag.Name).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, out)
	}
	return nil
}

// forgeTag reads the forge's object at the release tag, "" when absent.
func (p *releaseTagPush) forgeTag() string {
	out, err := exec.Command(p.f.Binary, "-C", p.f.Bare, "rev-parse", "--verify", "--quiet", p.tag.Name).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func TestAdmittedReleaseTagIsOnTheForgeBeforeDelivery(t *testing.T) {
	t.Parallel()
	p := newReleaseTagPush(t)
	delivery, err := p.receive(t, "", p.tag)
	if delivery != nil || err != nil {
		t.Fatal(delivery, err, p.output.String())
	}
	if p.delivered != 1 || p.forgeThen != p.tag.ObjectSHA {
		t.Fatalf("forge held %q when the run was created, want the pushed tag %s", p.forgeThen, p.tag.ObjectSHA)
	}
	if peeled := p.f.Git(t, p.f.Bare, "rev-parse", p.tag.Name+"^{commit}"); peeled != p.tag.CommitSHA {
		t.Fatal("forge tag peels to another commit", peeled)
	}
}

func TestIdenticalForgeTagCountsAsDelivered(t *testing.T) {
	t.Parallel()
	p := newReleaseTagPush(t)
	delivery, err := p.receive(t, p.tag.Name, p.tag)
	if delivery != nil || err != nil || p.delivered != 1 || p.forgeThen != p.tag.ObjectSHA {
		t.Fatal("identical forge tag was not accepted as delivered", delivery, err, p.output.String())
	}
}

func TestReleaseTagDeliveryRefusalsMoveNothing(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		forgeFirst string
		ref        func(*releaseTagPush) gitport.Ref
		want       func(error) bool
	}{
		"another tag already on the forge": {
			forgeFirst: "refs/tags/other",
			ref:        func(p *releaseTagPush) gitport.Ref { return p.tag },
			want: func(err error) bool {
				var exists *gitport.AlreadyExistsError
				return errors.As(err, &exists)
			},
		},
		"commit is not the tag's": {
			ref: func(p *releaseTagPush) gitport.Ref {
				ref := p.tag
				ref.CommitSHA = p.f.Commit
				return ref
			},
			want: func(err error) bool { return strings.Contains(err.Error(), "push commit does not match its object") },
		},
		"tag is not this push's": {
			ref: func(p *releaseTagPush) gitport.Ref {
				ref := p.tag
				ref.Name = "refs/tags/v9.9.9"
				return ref
			},
			want: func(err error) bool { return strings.Contains(err.Error(), "tags cannot move or be deleted") },
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := newReleaseTagPush(t)
			p.f.Git(t, p.f.Work, "tag", "-a", "other", "-m", "another object", p.f.Commit)
			before := ""
			if tc.forgeFirst != "" {
				before = p.f.Git(t, p.f.Work, "rev-parse", tc.forgeFirst)
			}
			delivery, err := p.receive(t, tc.forgeFirst, tc.ref(p))
			if err == nil || delivery == nil || !tc.want(delivery) {
				t.Fatalf("delivery refusal %v, receive %v", delivery, err)
			}
			if p.delivered != 0 || p.forgeTag() != before {
				t.Fatal("refused delivery moved the forge tag or reached delivery")
			}
		})
	}
}

func TestDeliveredTagIsNotRefusedPastTheAdmissionDeadline(t *testing.T) {
	t.Parallel()
	for _, deliver := range []bool{true, false} {
		t.Run(map[bool]string{true: "delivered", false: "nothing delivered"}[deliver], func(t *testing.T) {
			p := newReleaseTagPush(t)
			err := p.gate(t, 2*time.Second, func(ctx context.Context, s *gitcache.ReceiveSource) error {
				if deliver {
					if err := s.DeliverTag(ctx, p.tag); err != nil {
						return err
					}
				}
				<-ctx.Done()
				return nil
			})
			if deliver && (err != nil || p.delivered != 1 || p.forgeThen != p.tag.ObjectSHA) {
				t.Fatal("a delivered tag was refused at the deadline, spending its version", err, p.output.String())
			}
			if !deliver && (err == nil || p.delivered != 0) {
				t.Fatal("a late approval without delivery passed the deadline", err)
			}
		})
	}
}
