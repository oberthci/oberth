package gitcache_test

import (
	"context"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
)

func TestReceivePreservesLiteralTemplateRef(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	ref := "refs/heads/{{workflow.name}}"
	f.Git(t, f.Work, "check-ref-format", ref)
	f.Write(t, "file.txt", []byte("updated source\n"))
	next := f.CommitFile(t, "literal template ref")
	admitted, delivered := 0, 0
	cache := receiveCache(t, f, func(_ context.Context, event gitcache.ReceiveEvent, _ *gitcache.ReceiveSource) error {
		if len(event.Updates) != 1 || event.Updates[0].Ref != ref {
			t.Error("admission changed the literal ref", event.Updates)
		}
		admitted++
		return nil
	})
	request := gateReceive(f)
	request.Input = receiveBatchInput(t, f, []gitcache.RefUpdate{{Ref: ref, NewSHA: next}}, []string{"oberth.run=1"})
	request.Deliver = func(_ context.Context, event gitcache.ReceiveEvent) ([]string, error) {
		if len(event.Updates) != 1 || event.Updates[0].Ref != ref {
			t.Error("delivery changed the literal ref", event.Updates)
		}
		delivered++
		return nil, nil
	}
	if err := cache.Receive(t.Context(), request); err != nil {
		t.Fatal("ordinary push refused", err)
	}
	if admitted != 1 || delivered != 1 {
		t.Fatal("push did not admit and deliver exactly once", admitted, delivered)
	}
	if got := f.Git(t, f.Store(), "for-each-ref", "--format=%(refname)", ref); got != ref {
		t.Fatal("push changed literal ref", got)
	}
}
