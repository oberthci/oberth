package service

import (
	"context"
	"fmt"
	"testing"

	"github.com/oberthci/oberth/internal/model"
)

type promotionPinGit struct {
	*controlGit
	release func(context.Context, string, string, string) error
}

func (g *promotionPinGit) ReleasePromotionPin(ctx context.Context, repo, target, sha string) error {
	return g.release(ctx, repo, target, sha)
}

func TestAbandonPublicationReleasesExactPromotionPinAfterCommit(t *testing.T) {
	f := newControlFixture(t)
	promotion, pub := beginPendingPromotionPublication(t, f, "main", retryBaseSHA, retrySourceSHA)
	calls := 0
	f.scheduler.git = &promotionPinGit{controlGit: f.git, release: func(ctx context.Context, repo, target, sha string) error {
		state, err := f.store.PublicationState(ctx, pub.ID)
		if err != nil || state.Publication.Status != model.PublicationAbandoned {
			return fmt.Errorf("cleanup before commit: %+v %v", state, err)
		}
		upstream, err := f.store.Upstream(ctx, f.repo.UpstreamID)
		if err != nil {
			return err
		}
		if repo != upstream.QualifiedRepo(f.repo.Name) || target != promotion.TargetRef || sha != pub.ResultSHA {
			return fmt.Errorf("wrong pin: %s %s %s", repo, target, sha)
		}
		calls++
		return nil
	}}
	if _, err := f.scheduler.AbandonPublication(t.Context(), pub.ID, "admin@host", "stop promotion delivery"); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("released %d pins, want exactly the abandoned promotion", calls)
	}
	if len(f.git.promotions) != 0 {
		t.Fatal("abandon pushed promotion")
	}
}
