package gitcache_test

import (
	"testing"

	"github.com/oberthci/oberth/internal/gitcache/gittest"
)

func TestTopicPublicationKeepsReceiveHeadAcrossFetch(t *testing.T) {
	p := newPublishFixture(t)
	p.upstream(t, p.f.Commit)
	gittest.ExpireRefresh(t, p.f.Store())
	if err := p.cache.Fetch(t.Context(), gittest.Repository); err != nil {
		t.Fatal(err)
	}
	p.f.Git(t, p.f.Store(), "update-ref", p.intent.Ref, p.next)
	p.intent.ReceivedSHA = new(p.f.Commit)
	p.upstream(t, p.unrelated)
	gittest.ExpireRefresh(t, p.f.Store())
	if err := p.cache.Fetch(t.Context(), gittest.Repository); err != nil {
		t.Fatal(err)
	}
	// Even a live service lease must not become receive evidence.
	p.intent.PreviousSHA = new(p.unrelated)
	_, err := p.cache.Publish(t.Context(), p.intent)
	if publicationClass(err) != "moved(upstream)" {
		t.Fatal("refresh authorized an unseen topic rewrite", err)
	}
	if head := p.f.Git(t, p.f.Bare, "rev-parse", p.intent.Ref); head != p.unrelated {
		t.Fatal("unseen forge commit was overwritten", head)
	}
}

func TestTopicPublicationExtendsAnUntrackedForgeHead(t *testing.T) {
	p := newPublishFixture(t)
	p.upstream(t, p.next)
	p.f.Git(t, p.f.Work, "checkout", "-B", "tested", p.next)
	p.f.Write(t, "child", []byte("built on the forge head"))
	tested := p.f.CommitFile(t, "child of untracked head")
	p.f.Git(t, p.f.Store(), "fetch", p.f.Work, tested)
	p.f.Git(t, p.f.Store(), "update-ref", p.intent.Ref, tested)
	p.object(tested)
	p.predecessor(p.f.Commit)
	if _, err := p.cache.Publish(t.Context(), p.intent); err != nil {
		t.Fatal("fast-forward over an untracked forge head was refused", err)
	}
	if head := p.f.Git(t, p.f.Bare, "rev-parse", p.intent.Ref); head != tested {
		t.Fatal("fast-forward did not publish", head)
	}
}

func TestTopicPublicationRefusesDeletedAndReplacedReceiveHeads(t *testing.T) {
	for _, change := range []string{"deleted", "recreated", "force-pushed"} {
		t.Run(change, func(t *testing.T) {
			p := newPublishFixture(t)
			p.upstream(t, p.f.Commit)
			p.predecessor(p.f.Commit)
			if change != "force-pushed" {
				p.f.Git(t, p.f.Bare, "update-ref", "-d", p.intent.Ref)
			}
			if change != "deleted" {
				p.upstream(t, p.unrelated)
			}
			before := p.f.Git(t, p.f.Bare, "for-each-ref", "--format=%(objectname)", p.intent.Ref)
			gittest.ExpireRefresh(t, p.f.Store())
			if err := p.cache.Fetch(t.Context(), gittest.Repository); err != nil {
				t.Fatal(err)
			}
			_, err := p.cache.Publish(t.Context(), p.intent)
			if publicationClass(err) != "moved(upstream)" {
				t.Fatal("publication replaced a deleted or moved receive head", err)
			}
			if head := p.f.Git(t, p.f.Bare, "for-each-ref", "--format=%(objectname)", p.intent.Ref); head != before {
				t.Fatal("refusal changed the forge", head)
			}
		})
	}
}
