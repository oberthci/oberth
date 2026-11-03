package service

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/oberthci/oberth/internal/api"
	"strings"
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
			return fmt.Errorf("cleanup before commit: %+v %w", state, err)
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

func TestAbandonPublicationCleanupFailureWarnsAndCanRetry(t *testing.T) {
	f := newControlFixture(t)
	_, pub := beginPendingPromotionPublication(t, f, "main", retryBaseSHA, retrySourceSHA)
	upstream, err := f.store.Upstream(t.Context(), f.repo.UpstreamID)
	if err != nil {
		t.Fatal(err)
	}
	qualified := upstream.QualifiedRepo(f.repo.Name)
	if abandoned, err := f.store.AbandonedPromotionPin(t.Context(), qualified, pub.Ref, pub.ResultSHA); err != nil || abandoned {
		t.Fatalf("pending pin: %t %v", abandoned, err)
	}
	calls := 0
	f.scheduler.git = &promotionPinGit{controlGit: f.git, release: func(context.Context, string, string, string) error {
		calls++
		if calls == 1 {
			return fmt.Errorf("cache lock timed out")
		}
		return nil
	}}
	control := f.api(t)
	args, _ := json.Marshal(map[string]string{"id": pub.ID, "reason": "stop promotion delivery"})
	value, err := control.CallTool(t.Context(), api.Actor{Identity: "admin@host", Admin: true}, "publication_abandon", args)
	if err != nil {
		t.Fatalf("committed abandonment must succeed with a warning: %v", err)
	}
	response := value.(api.PublicationAbandonResponse)
	if response.Status != "abandoned" || !strings.Contains(response.Message, "cleanup") || !strings.Contains(response.Message, "retry") {
		t.Fatalf("missing cleanup retry guidance: %+v", response)
	}
	for _, tc := range []struct {
		target, sha string
		want        bool
	}{{pub.Ref, pub.ResultSHA, true}, {"other", pub.ResultSHA, false}, {pub.Ref, retryBaseSHA, false}} {
		if abandoned, err := f.store.AbandonedPromotionPin(t.Context(), qualified, tc.target, tc.sha); err != nil || abandoned != tc.want {
			t.Fatalf("abandoned pin: %t %v, want %t", abandoned, err, tc.want)
		}
	}
	state, err := f.store.PublicationState(t.Context(), pub.ID)
	if err != nil || state.Publication.Status != model.PublicationAbandoned {
		t.Fatalf("committed state: %+v %v", state, err)
	}
}
