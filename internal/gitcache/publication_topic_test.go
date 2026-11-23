package gitcache_test

import (
	"errors"
	"testing"

	"github.com/oberthci/oberth/internal/gitport"
)

func TestRunPublicationRefusesExternalBranchRewind(t *testing.T) {
	p := newPublishFixture(t)
	p.intent.RunID = "run"
	// Bind the observed predecessor before the independent forge change.
	seen, readErr := p.cache.ObservedRef(t.Context(), p.intent.Repo, p.intent.Ref)
	var absent *gitport.AbsentError
	if readErr != nil && !errors.As(readErr, &absent) {
		t.Fatal(readErr)
	}
	p.predecessor(seen.ObjectSHA)
	p.upstream(t, p.unrelated)
	_, err := p.cache.Publish(t.Context(), p.intent)
	if publicationClass(err) != "moved(upstream)" {
		t.Fatal("CI publication replaced independent forge history", err)
	}
	if actual := p.f.Git(t, p.f.Bare, "rev-parse", p.intent.Ref); actual != p.unrelated {
		t.Fatal("CI publication rewrote the external branch", actual)
	}
}
