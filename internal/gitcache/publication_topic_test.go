package gitcache_test

import (
	"testing"
)

func TestRunPublicationRefusesExternalBranchRewind(t *testing.T) {
	p := newPublishFixture(t)
	p.intent.RunID = "run"
	// Bind the observed predecessor before the independent forge change.
	p.intent.ReceivedHeads = []string{""}
	p.upstream(t, p.unrelated)
	_, err := p.cache.Publish(t.Context(), p.intent)
	if publicationClass(err) != "moved(upstream)" {
		t.Fatal("CI publication replaced independent forge history", err)
	}
	if actual := p.f.Git(t, p.f.Bare, "rev-parse", p.intent.Ref); actual != p.unrelated {
		t.Fatal("CI publication rewrote the external branch", actual)
	}
}
