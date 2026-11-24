package app_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestExplicitRetryConfirmsLandedPublicationAfterLocalMove(t *testing.T) {
	f := newMovingFixtureFrom(t, gittest.New(t))
	g := f.git
	g.Git(t, g.Store(), "update-ref", "refs/heads/topic", g.Commit)
	p := model.Publication{ID: "landed-retry", Repo: request().Repository, Ref: "refs/heads/topic", Actor: movingCaller().Actor.Identity, ObjectSHA: g.Commit, CommitSHA: new(g.Commit)}
	landedOK(t, f.db.PreparePublication(t.Context(), stateport.Action{Actor: p.Actor}, stateport.PublicationIntent{Publication: p}))
	lost := &lostPublicationReply{MovingGit: f.moving.MovingGit}
	f.moving.MovingGit = lost
	landedOK(t, f.svc.Deliver(t.Context(), p.ID))
	deferred, err := f.db.Publication(t.Context(), p.ID)
	if err != nil || deferred.Status != model.PublicationPending || deferred.RetryAt == nil {
		t.Fatal("lost reply did not defer the landed publication", deferred, err)
	}
	g.Write(t, "newer", []byte("a newer accepted push"))
	newer := g.CommitFile(t, "newer local head")
	g.Git(t, g.Store(), "fetch", "--no-auto-maintenance", g.Work, newer)
	g.Git(t, g.Store(), "update-ref", p.Ref, newer)
	call, err := f.svc.PreparePublishRetry(t.Context(), movingCaller(), apicontract.PublishRetryArgs{ID: p.ID, Reason: "confirm the landed push"})
	landedOK(t, err)
	_, err = call(t.Context())
	landedOK(t, err)
	landedOK(t, f.svc.Deliver(t.Context(), p.ID))
	stored, err := f.db.Publication(t.Context(), p.ID)
	landedOK(t, err)
	lost.mu.Lock()
	modes := slices.Clone(lost.modes)
	lost.mu.Unlock()
	if stored.Status != model.PublicationDelivered || stored.Attempts != 1 || stored.ReadBack || !slices.Equal(modes, []bool{false, true}) {
		t.Fatal("explicit retry lost a landed publication", stored, modes)
	}
	if actual := g.Git(t, g.Bare, "rev-parse", p.Ref); actual != p.ObjectSHA {
		t.Fatal("confirming the retry changed the landed forge head", actual)
	}
	if actual := g.Git(t, g.Store(), "rev-parse", p.Ref); actual != newer {
		t.Fatal("confirming the retry replaced the newer local head", actual)
	}
}

type lostPublicationReply struct {
	service.MovingGit
	mu    sync.Mutex
	modes []bool
}

func (g *lostPublicationReply) Publish(ctx context.Context, p model.Publication) (gitport.PublicationReceipt, error) {
	g.mu.Lock()
	g.modes = append(g.modes, p.ReadBack)
	first := len(g.modes) == 1
	g.mu.Unlock()
	receipt, err := g.MovingGit.Publish(ctx, p)
	if err == nil && first {
		return gitport.PublicationReceipt{}, &gitport.UnavailableError{Op: "forge", Cause: errors.New("reply lost after a landed push")}
	}
	return receipt, err
}
