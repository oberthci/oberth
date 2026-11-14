package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/model"
)

func TestParkPromotionRefusesRedBranchSHAForSingleAndTrain(t *testing.T) {
	for _, status := range []model.RunStatus{model.RunFailed, model.RunInterrupted} {
		for _, train := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/train=%v", status, train), func(t *testing.T) {
				f := newControlFixture(t)
				sha := strings.Repeat("a", 40)
				queued, err := f.scheduler.recordCIForTest(t.Context(), CIRequest{EventID: "red", Repository: f.repo, Branch: "feature/red", SHA: sha, Actor: "pusher"})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.store.FinishRun(t.Context(), queued.ID, model.RunResult{Status: status, Phase: "test", FailedStage: "test", FailedStep: "unit"}); err != nil {
					t.Fatal(err)
				}
				s := f.api(t)
				defer func() { _ = s.Shutdown(context.Background()) }()
				s.git = &parkTrainGit{parkDeliveryGit: &parkDeliveryGit{DeliveryGit: f.git, ref: "park/review"}, train: &trainControlGit{controlGit: f.git}}
				var request string
				if train {
					request = fmt.Sprintf(`{"repo":%q,"branch":"main","sources":[{"sha":%q}]}`, f.repo.Name, sha)
				} else {
					request = fmt.Sprintf(`{"repo":%q,"branch":"main","sha":%q}`, f.repo.Name, sha)
				}
				_, err = s.CallTool(t.Context(), api.Actor{Identity: "admin", Admin: true}, "promote", json.RawMessage(request))
				if err == nil || !strings.Contains(err.Error(), "red branch run "+queued.ID) {
					t.Fatalf("red evidence discarded: %v", err)
				}
				promotions, err := f.store.ListPromotions(t.Context(), model.PromotionListFilter{RepoID: f.repo.ID})
				if err != nil || len(promotions.Promotions) != 0 {
					t.Fatal("red source admitted", promotions, err)
				}
			})
		}
	}
}

type parkTrainGit struct {
	*parkDeliveryGit
	train *trainControlGit
}

func (g *parkTrainGit) PreparePromotionTrain(ctx context.Context, repo string, sources []model.PromotionSource, target, workspace string, skip bool) (gitcache.PromotionTrainCandidate, error) {
	return g.train.PreparePromotionTrain(ctx, repo, sources, target, workspace, skip)
}
