package gitcache_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/model"
)

type publishFixture struct {
	f                            *gittest.Fixture
	cache                        *gitcache.Cache
	next, unrelated, tag, oldTag string
	intent                       model.Publication
}

func newPublishFixture(t *testing.T) *publishFixture {
	t.Helper()
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	cfg.Binary = reviewWrapper(t, f)
	p := &publishFixture{f: f, cache: reviewNew(t, cfg)}
	if err := p.cache.Init(t.Context(), gittest.Repository); err != nil {
		t.Fatal(err)
	}
	f.Git(t, f.Work, "checkout", "-b", "topic")
	f.Write(t, "topic", []byte("topic"))
	p.next = f.CommitFile(t, "topic")
	f.Git(t, f.Work, "checkout", "--orphan", "unrelated")
	f.Write(t, "unrelated", []byte("unrelated"))
	p.unrelated = f.CommitFile(t, "unrelated")
	f.Git(t, f.Work, "tag", "-a", "release", "-m", "release", p.next)
	f.Git(t, f.Work, "tag", "-a", "old-release", "-m", "old", f.Commit)
	p.tag = f.Git(t, f.Work, "rev-parse", "release")
	p.oldTag = f.Git(t, f.Work, "rev-parse", "old-release")
	f.Git(t, f.Store(), "fetch", f.Work, "+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*")
	p.intent = publicationFor(f, "refs/heads/topic", p.next, "")
	return p
}
func (p *publishFixture) upstream(t *testing.T, oid string) {
	t.Helper()
	p.f.Git(t, p.f.Bare, "fetch", p.f.Work, oid)
	p.f.Git(t, p.f.Bare, "update-ref", p.intent.Ref, oid)
}
func (p *publishFixture) object(oid string) {
	p.intent.ObjectSHA = oid
	p.intent.CommitSHA = &oid
}
func (p *publishFixture) predecessor(oid string) { p.intent.PreviousSHA = &oid }
func (p *publishFixture) releaseTag() {
	p.intent.Ref = "refs/tags/release"
	p.intent.ObjectSHA = p.tag
	p.intent.CommitSHA = &p.next
}

type publicationCase struct {
	name, class           string
	contains              string
	change                func(*testing.T, *publishFixture)
	noPush, missingRemote bool
}

// These are the supplied publication-class probes, with assertions on classes,
// side effects and native process boundaries instead of diagnostic-only logs.
func TestPublishRefusalClassesAndRemoteEffects(t *testing.T) {
	t.Parallel()
	cases := append(publicationBranchCases(), publicationTagCases()...)
	cases = append(cases, publicationFailureCases()...)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { checkPublicationCase(t, tc) })
	}
}
func checkPublicationCase(t *testing.T, tc publicationCase) {
	t.Helper()
	p := newPublishFixture(t)
	if tc.change != nil {
		tc.change(t, p)
	}
	before := ""
	if !tc.missingRemote {
		before = p.f.Git(t, p.f.Bare, "for-each-ref", "--format=%(objectname)", p.intent.Ref)
	}
	if err := os.WriteFile(filepath.Join(p.f.Root, "commands.jsonl"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	_, err := p.cache.Publish(t.Context(), p.intent)
	if got := publicationClass(err); got != tc.class {
		t.Fatalf("want %s, got %s: %v", tc.class, got, err)
	}
	if tc.contains != "" && !strings.Contains(err.Error(), tc.contains) {
		t.Fatal("push refusal did not explain the cause", err)
	}
	if tc.noPush && publicationPushes(t, p.f) != 0 {
		t.Fatal("preflight refusal or retry started a native push")
	}
	if tc.missingRemote {
		return
	}
	expected := before
	if tc.class == "ok" {
		expected = p.intent.ObjectSHA
	}
	actual := p.f.Git(t, p.f.Bare, "for-each-ref", "--format=%(objectname)", p.intent.Ref)
	if actual != expected {
		t.Fatalf("upstream changed incorrectly: want %q, got %q", expected, actual)
	}
}
func publicationClass(err error) string {
	var moved *gitport.RefMovedError
	var exists *gitport.AlreadyExistsError
	var unavailable *gitport.UnavailableError
	var refused *gitcache.RefusalError
	switch {
	case err == nil:
		return "ok"
	case errors.As(err, &moved):
		return "moved(" + moved.Location + ")"
	case errors.As(err, &exists):
		return "already-exists"
	case errors.As(err, &unavailable):
		return "unavailable"
	case errors.As(err, &refused):
		return "refused"
	default:
		return fmt.Sprintf("other(%T)", err)
	}
}
func publicationPushes(t *testing.T, f *gittest.Fixture) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.Root, "commands.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var command struct {
			Argv []string `json:"argv"`
		}
		if err = json.Unmarshal([]byte(line), &command); err != nil {
			t.Fatal(err)
		}
		for _, arg := range command.Argv {
			if arg == "push" {
				count++
			}
		}
	}
	return count
}
func publicationBranchCases() []publicationCase {
	return []publicationCase{
		{name: "create", class: "ok"},
		{name: "identical retry", class: "ok",
			noPush: true, change: func(t *testing.T, p *publishFixture) { p.upstream(t, p.next) }},
		{name: "upstream moved", class: "moved(upstream)", change: func(t *testing.T, p *publishFixture) {
			p.upstream(t, p.f.Commit)
			p.predecessor(p.unrelated)
		}},
		{name: "accepted local head moved", class: "moved(local)",
			noPush: true, change: func(t *testing.T, p *publishFixture) {
				p.f.Git(t, p.f.Store(), "update-ref", p.intent.Ref, p.f.Commit)
			}},
		{name: "default branch non fast forward", class: "refused",
			contains: "push is not a fast-forward",
			noPush:   true, change: func(t *testing.T, p *publishFixture) {
				p.intent.Ref = "refs/heads/main"
				p.object(p.unrelated)
				p.predecessor(p.f.Commit)
				p.f.Git(t, p.f.Store(), "update-ref", p.intent.Ref, p.unrelated)
			}},
		{name: "ordinary branch exact rewrite", class: "ok", change: func(t *testing.T, p *publishFixture) {
			p.upstream(t, p.f.Commit)
			p.object(p.unrelated)
			p.predecessor(p.f.Commit)
			p.f.Git(t, p.f.Store(), "update-ref", p.intent.Ref, p.unrelated)
		}},
		{name: "runless deletion", class: "ok", change: func(t *testing.T, p *publishFixture) {
			p.upstream(t, p.f.Commit)
			p.predecessor(p.f.Commit)
			p.intent.ObjectSHA = ""
			p.intent.CommitSHA = nil
			p.f.Git(t, p.f.Store(), "update-ref", "-d", p.intent.Ref)
		}},
		{name: "branch commit mismatch before identical retry", class: "refused",
			contains: "push commit does not match its object", noPush: true,
			change: func(t *testing.T, p *publishFixture) { p.upstream(t, p.next); p.intent.CommitSHA = &p.f.Commit }},
	}
}
func publicationTagCases() []publicationCase {
	return []publicationCase{
		{name: "annotated tag create", class: "ok", change: func(_ *testing.T, p *publishFixture) { p.releaseTag() }},
		{name: "tag already exists before push", class: "already-exists", noPush: true,
			change: func(t *testing.T, p *publishFixture) { p.releaseTag(); p.upstream(t, p.oldTag) }},
		{name: "tag move", class: "refused",
			noPush: true, change: func(t *testing.T, p *publishFixture) {
				p.releaseTag()
				p.upstream(t, p.oldTag)
				p.predecessor(p.oldTag)
			}},
		{name: "tag delete", class: "refused",
			noPush: true, change: func(t *testing.T, p *publishFixture) {
				p.releaseTag()
				p.upstream(t, p.oldTag)
				p.predecessor(p.oldTag)
				p.intent.ObjectSHA = ""
				p.intent.CommitSHA = nil
			}},
		{name: "tag peeled commit mismatch", class: "refused",
			contains: "push commit does not match its object",
			noPush:   true, change: func(_ *testing.T, p *publishFixture) {
				p.releaseTag()
				p.intent.CommitSHA = &p.f.Commit
			}},
	}
}
func publicationFailureCases() []publicationCase {
	return []publicationCase{
		{name: "upstream hook policy", class: "refused",
			contains: "upstream refused push", change: func(t *testing.T, p *publishFixture) {
				hook := filepath.Join(p.f.Bare, "hooks", "pre-receive")
				if err := os.WriteFile(hook, []byte("#!/bin/sh\necho protected by policy >&2\nexit 1\n"), 0700); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "upstream branch rewrite protection", class: "refused", change: func(t *testing.T, p *publishFixture) {
			p.upstream(t, p.f.Commit)
			p.predecessor(p.f.Commit)
			p.object(p.unrelated)
			p.f.Git(t, p.f.Store(), "update-ref", p.intent.Ref, p.unrelated)
			p.f.Git(t, p.f.Bare, "config", "receive.denyNonFastForwards", "true")
		}},
		{name: "upstream deletion protection", class: "refused", change: func(t *testing.T, p *publishFixture) {
			p.upstream(t, p.f.Commit)
			p.predecessor(p.f.Commit)
			p.intent.ObjectSHA = ""
			p.intent.CommitSHA = nil
			p.f.Git(t, p.f.Store(), "update-ref", "-d", p.intent.Ref)
			p.f.Git(t, p.f.Bare, "config", "receive.denyDeletes", "true")
		}},
		{name: "upstream cannot store pack", class: "unavailable", change: func(t *testing.T, p *publishFixture) {
			p.f.Git(t, p.f.Bare, "config", "receive.maxInputSize", "1")
		}},
		{name: "upstream unreachable", class: "unavailable", missingRemote: true,
			change: func(t *testing.T, p *publishFixture) {
				if err := os.Rename(p.f.Bare, p.f.Bare+".off"); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "missing prepared promotion", class: "refused",
			noPush: true,
			change: func(_ *testing.T, p *publishFixture) { p.intent.PromotionID = "missing" }},
		{name: "promotion non fast forward", class: "refused",
			contains: "push is not a fast-forward",
			noPush:   true, change: func(t *testing.T, p *publishFixture) {
				prepared, err := p.cache.PreparePromotion(t.Context(), gitport.PromotionRequest{Repository: gittest.Repository,
					Source: gitport.Revision{Repository: gittest.Repository, Commit: p.unrelated},
					Target: gitport.Ref{Name: "refs/heads/new-target"}, ActionReceipt: "non-forward"})
				if err != nil {
					t.Fatal(err)
				}
				p.upstream(t, p.f.Commit)
				p.object(prepared.Result.ObjectSHA)
				p.predecessor(p.f.Commit)
				p.intent.PromotionID = prepared.Request.ActionReceipt
			}},
	}
}
