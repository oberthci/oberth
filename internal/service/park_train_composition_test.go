package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/model"
)

func TestPromotionTrainAcceptsParkSourcesWithPerSourceAuthority(t *testing.T) {
	for _, tc := range []struct {
		name, policy             string
		admin, parkedGrant, want bool
	}{
		{"admin", "", true, false, true},
		{"repository opt-in", "allow_without_branch_ci: true\n", false, false, true},
		{"exact grants", "", false, true, true},
		{"missing park grant", "", false, false, false},
		{"protected target", "require_branch_ci: [main]\n", true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newControlFixture(t)
			green, parked := strings.Repeat("1", 40), strings.Repeat("2", 40)
			seedGreenPromotionCandidate(t, f, green)
			if !tc.admin {
				shas := []string{green}
				if tc.parkedGrant {
					shas = append(shas, parked)
				}
				for _, sha := range shas {
					if _, err := f.store.AppendPromotionGrant(t.Context(), model.PromotionGrantSpec{RepoID: f.repo.ID, SourceSHA: sha, TargetRef: "main", Issuer: "admin", IssuerFingerprint: "SHA256:admin", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
						t.Fatal(err)
					}
				}
			}
			s := f.api(t)
			s.promotionGrants = f.store
			defer func() { _ = s.Shutdown(context.Background()) }()
			f.git.plan = gitcache.MergeCandidate{BaseSHA: strings.Repeat("b", 40), MergedSHA: strings.Repeat("c", 40)}
			s.git = &parkTrainGit{parkDeliveryGit: &parkDeliveryGit{DeliveryGit: f.git, ref: "park/review", policy: tc.policy}, train: &trainControlGit{controlGit: f.git}}
			args := json.RawMessage(fmt.Sprintf(`{"repo":%q,"branch":"main","sources":[{"sha":%q},{"sha":%q,"ref":"park/review"}]}`, f.repo.Name, green, parked))
			value, err := s.CallTool(t.Context(), api.Actor{Identity: "caller", Admin: tc.admin}, "promote", args)
			if (err == nil) != tc.want {
				t.Fatalf("mixed train admission: %v", err)
			}
			if !tc.want {
				return
			}
			p := requireToolPromotion(t, f, value)
			if !p.BranchCISkipped || p.RunID == "" || p.Train == nil || p.SourceCount() != 2 || p.Train.Sources[1].Ref != "park/review" {
				t.Fatalf("parked train evidence lost: %+v", p)
			}
			run, err := f.store.Run(t.Context(), p.RunID)
			if err != nil || !strings.Contains(run.Reason, "promoted without branch CI") {
				t.Fatal("missing reason", run, err)
			}
			audit, err := f.store.LatestAuditActionByType(t.Context(), "promotion.admit", "promotion")
			if err != nil || !strings.Contains(audit.Details, `"branch_ci_skipped":"true"`) || !strings.Contains(audit.Details, `"source_count":2`) {
				t.Fatal("missing mixed train audit", audit, err)
			}
		})
	}
}

func TestMixedParkTrainRunsOneGateAndNeverPublishesRed(t *testing.T) {
	for _, green := range []bool{false, true} {
		t.Run(fmt.Sprint(green), func(t *testing.T) {
			result := JobResult{Status: model.RunPassed, Phase: "passed", Steps: []model.StepResult{stepResult(model.StepPassed)}}
			if !green {
				result = JobResult{Status: model.RunFailed, Phase: "test", FailedStage: "test", FailedStep: "unit", Steps: []model.StepResult{stepResult(model.StepFailed)}}
			}
			f := newControlFixture(t, result)
			a, b := strings.Repeat("1", 40), strings.Repeat("2", 40)
			seedGreenPromotionCandidate(t, f, a)
			f.git.plan = gitcache.MergeCandidate{BaseSHA: strings.Repeat("b", 40), MergedSHA: strings.Repeat("c", 40)}
			s := f.api(t)
			defer func() { _ = s.Shutdown(context.Background()) }()
			s.git = &parkTrainGit{parkDeliveryGit: &parkDeliveryGit{DeliveryGit: f.git, ref: "park/review"}, train: &trainControlGit{controlGit: f.git}}
			value, err := s.CallTool(t.Context(), api.Actor{Identity: "admin", Admin: true}, "promote", json.RawMessage(fmt.Sprintf(`{"repo":%q,"branch":"main","sources":[{"sha":%q},{"sha":%q}]}`, f.repo.Name, a, b)))
			if err != nil {
				t.Fatal(err)
			}
			p := requireToolPromotion(t, f, value)
			if len(f.git.promotions) != 0 {
				t.Fatal("published before CI")
			}
			if err := f.scheduler.ProcessNext(t.Context()); err != nil {
				t.Fatal(err)
			}
			finished, err := f.store.Promotion(t.Context(), p.ID)
			if err != nil || len(f.jobs.createdCI) != 1 || (finished.Status == model.PromotionPassed) != green || (len(f.git.promotions) == 1) != green {
				t.Fatalf("train bypassed combined CI: %+v %v", finished, err)
			}
		})
	}
}
