package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/model"
)

type trainControlGit struct {
	*controlGit
	calls int
}

func (g *trainControlGit) PreparePromotionTrain(_ context.Context, _ string, sources []model.PromotionSource, target, _ string, _ bool) (gitcache.PromotionTrainCandidate, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls++
	if g.plan.BaseSHA != "" {
		g.setRemoteRefLocked("refs/heads/"+target, g.plan.BaseSHA)
	}
	return gitcache.PromotionTrainCandidate{MergeCandidate: g.plan, TreeSHA: strings.Repeat("f", 40), Sources: append([]model.PromotionSource(nil), sources...)}, nil
}

func TestPromotionTrainAdmissionQueueAndRetry(t *testing.T) {
	f := newControlFixture(t)
	ctx := t.Context()
	a, b := strings.Repeat("1", 40), strings.Repeat("2", 40)
	seedGreenPromotionCandidate(t, f, a)
	seedGreenPromotionCandidate(t, f, b)
	c := f.api(t)
	defer func() { _ = c.Shutdown(context.Background()) }()
	g := &trainControlGit{controlGit: f.git}
	c.git = g
	g.plan = gitcache.MergeCandidate{BaseSHA: strings.Repeat("b", 40), MergedSHA: strings.Repeat("c", 40)}
	actor := api.Actor{Identity: "admin@host", Admin: true}
	args := json.RawMessage(fmt.Sprintf(`{"branch":"main","sources":[{"sha":%q},{"sha":%q}]}`, a, b))
	value, err := c.CallTool(ctx, actor, "promote", args)
	if err != nil {
		t.Fatal(err)
	}
	first := requireToolPromotion(t, f, value)
	if first.Train == nil || first.SourceCount() != 2 || first.RunID == "" || first.Train.TreeSHA != strings.Repeat("f", 40) {
		t.Fatalf("train = %+v", first)
	}
	// Another train is planned immediately on the same base.
	value, err = c.CallTool(ctx, actor, "promote", args)
	if err != nil {
		t.Fatal(err)
	}
	second := requireToolPromotion(t, f, value)
	if second.RunID == "" || second.PreviousSHA != first.PreviousSHA {
		t.Fatalf("second train not planned immediately: %+v", second)
	}
	g.mu.Lock()
	calls := g.calls
	g.plan.BaseSHA = first.ResultSHA
	g.plan.MergedSHA = strings.Repeat("d", 40)
	g.mu.Unlock()
	if calls != 2 {
		t.Fatalf("prepared %d trains", calls)
	}
	if _, err = f.store.FinishRun(ctx, first.RunID, model.RunResult{Status: model.RunFailed, Phase: "job", Error: "unit failed"}); err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.FinishRun(ctx, second.RunID, model.RunResult{Status: model.RunFailed, Phase: "job", Error: "unit failed"}); err != nil {
		t.Fatal(err)
	}
	value, err = c.CallTool(ctx, actor, "promote_retry", json.RawMessage(fmt.Sprintf(`{"id":%q}`, second.ID)))
	if err != nil {
		t.Fatal(err)
	}
	retry, err := f.store.Promotion(ctx, value.(api.PromoteRetryResponse).ID)
	if err != nil {
		t.Fatal(err)
	}
	if retry.Train == nil || retry.SourceCount() != 2 || retry.Train.Sources[1].SHA != b || retry.RunID == "" {
		t.Fatalf("retry lost train: %+v", retry)
	}
	list, err := c.CallTool(ctx, actor, "promotion_list", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(list)
	if !strings.Contains(string(data), `"source_count":2`) {
		t.Fatalf("list = %s", data)
	}
	status, err := c.CallTool(ctx, actor, "promote_status", json.RawMessage(fmt.Sprintf(`{"id":%q,"timeout_s":0}`, first.ID)))
	if err != nil {
		t.Fatal(err)
	}
	data, _ = json.Marshal(status)
	if !strings.Contains(string(data), `"source_count":2`) {
		t.Fatalf("status = %s", data)
	}
}

func TestPromotionTrainRejectsInvalidMembersBeforeAdmission(t *testing.T) {
	for _, body := range []string{
		`{"sha":"%s","sources":[{"sha":"%s"}],"branch":"main"}`,
		`{"sources":[],"branch":"main"}`,
		`{"sources":[{"sha":"%s"},{"sha":"%s"}],"branch":"main"}`,
		`{"sources":[{"sha":"%s","ref":"wrong"}],"branch":"main"}`,
		`{"sources":[{"sha":"%s","grant_id":"forged"}],"branch":"main"}`,
		`{"sources":[{"sha":"%s"},{"sha":"2222222222222222222222222222222222222222"}],"branch":"main"}`,
	} {
		t.Run(body, func(t *testing.T) {
			f := newControlFixture(t)
			sha := strings.Repeat("1", 40)
			seedGreenPromotionCandidate(t, f, sha)
			c := f.api(t)
			c.git = &trainControlGit{controlGit: f.git}
			body = strings.ReplaceAll(body, "%s", sha)
			if _, err := c.CallTool(t.Context(), api.Actor{Identity: "admin@host", Admin: true}, "promote", json.RawMessage(body)); err == nil || errors.Is(err, ErrUnavailable) {
				t.Fatal("accepted invalid train")
			}
			page, err := f.store.ListPromotions(t.Context(), model.PromotionListFilter{})
			if err != nil || len(page.Promotions) != 0 {
				t.Fatalf("invalid train admitted: %+v %v", page, err)
			}
		})
	}
}

func TestPromotionTrainRunsOneGateBeforePublishing(t *testing.T) {
	for _, green := range []bool{false, true} {
		for _, unborn := range []bool{false, true} {
			t.Run(fmt.Sprintf("green=%v/unborn=%v", green, unborn), func(t *testing.T) {
				status := model.RunFailed
				step := model.StepFailed
				if green {
					status = model.RunPassed
					step = model.StepPassed
				}
				f := newControlFixture(t, JobResult{Status: status, Phase: "job", Steps: []model.StepResult{stepResult(step)}})
				a, b := strings.Repeat("1", 40), strings.Repeat("2", 40)
				seedGreenPromotionCandidate(t, f, a)
				seedGreenPromotionCandidate(t, f, b)
				c := f.api(t)
				g := &trainControlGit{controlGit: f.git}
				c.git = g
				g.plan = gitcache.MergeCandidate{BaseSHA: strings.Repeat("b", 40), MergedSHA: strings.Repeat("c", 40)}
				if unborn {
					g.plan.BaseSHA = ""
					g.plan.TargetUnborn = true
				}
				value, err := c.CallTool(t.Context(), api.Actor{Identity: "admin@host", Admin: true}, "promote", json.RawMessage(fmt.Sprintf(`{"branch":"main","sources":[{"sha":%q},{"sha":%q}]}`, a, b)))
				if err != nil {
					t.Fatal(err)
				}
				p := requireToolPromotion(t, f, value)
				if p.RunID == "" || p.Status != model.PromotionPending || len(g.promotions) != 0 {
					t.Fatalf("ungated train: %+v pushes=%v", p, g.promotions)
				}
				if err = f.scheduler.ProcessNext(t.Context()); err != nil {
					t.Fatal(err)
				}
				p, err = f.store.Promotion(t.Context(), p.ID)
				if err != nil {
					t.Fatal(err)
				}
				want := model.PromotionFailed
				pushes := 0
				if green {
					want = model.PromotionPassed
					pushes = 1
				}
				if p.Status != want || len(g.promotions) != pushes || len(f.jobs.createdCI) != 1 {
					t.Fatalf("train=%+v pushes=%v jobs=%d", p, g.promotions, len(f.jobs.createdCI))
				}
				if f.jobs.createdCI[0].Run.SHA != g.plan.MergedSHA {
					t.Fatal("CI tested the wrong tree")
				}
			})
		}
	}
}

type conflictingTrainGit struct{ *trainControlGit }

func (g *conflictingTrainGit) PreparePromotionTrain(ctx context.Context, input string, sources []model.PromotionSource, target, workspace string, skip bool) (gitcache.PromotionTrainCandidate, error) {
	plan, err := g.trainControlGit.PreparePromotionTrain(ctx, input, sources, target, workspace, skip)
	if err != nil {
		return plan, err
	}
	if !skip {
		plan.TreeSHA = ""
		plan.MergedSHA = ""
		return plan, &gitcache.PromotionTrainConflict{Index: 2, SHA: sources[1].SHA, Paths: []string{"file.txt"}}
	}
	plan.Sources[1].Skipped = true
	plan.Sources[1].ConflictPaths = []string{"file.txt"}
	return plan, nil
}

func TestPromotionTrainConflictEvidenceSurvivesSuccess(t *testing.T) {
	for _, skip := range []bool{false, true} {
		t.Run(fmt.Sprint(skip), func(t *testing.T) {
			f := newControlFixture(t, JobResult{Status: model.RunPassed, Phase: "passed", Steps: []model.StepResult{stepResult(model.StepPassed)}})
			a, b := strings.Repeat("1", 40), strings.Repeat("2", 40)
			seedGreenPromotionCandidate(t, f, a)
			seedGreenPromotionCandidate(t, f, b)
			c := f.api(t)
			c.git = &conflictingTrainGit{trainControlGit: &trainControlGit{controlGit: f.git}}
			f.git.plan = gitcache.MergeCandidate{BaseSHA: strings.Repeat("b", 40), MergedSHA: strings.Repeat("c", 40)}
			value, err := c.CallTool(t.Context(), api.Actor{Identity: "admin@host", Admin: true}, "promote", json.RawMessage(fmt.Sprintf(`{"branch":"main","skip_conflicting":%v,"sources":[{"sha":%q},{"sha":%q}]}`, skip, a, b)))
			if err != nil {
				t.Fatal(err)
			}
			p := requireToolPromotion(t, f, value)
			if !skip {
				if p.Status != model.PromotionFailed || p.RunID != "" || !strings.Contains(p.Error, "source 2") || !strings.Contains(p.Error, "file.txt") || len(f.jobs.createdCI) != 0 || len(f.git.promotions) != 0 {
					t.Fatalf("conflict started work: %+v", p)
				}
				return
			}
			if err = f.scheduler.ProcessNext(t.Context()); err != nil {
				t.Fatal(err)
			}
			p, err = f.store.Promotion(t.Context(), p.ID)
			if err != nil {
				t.Fatal(err)
			}
			if p.Status != model.PromotionPassed || !p.Train.Sources[1].Skipped || !strings.Contains(p.Error, b) || !strings.Contains(p.Error, "file.txt") || p.SourceCount() != 2 {
				t.Fatalf("skip evidence lost: %+v", p)
			}
		})
	}
}

func TestPromotionTrainScopedGrantAdmissionNeedsEverySource(t *testing.T) {
	f := newControlFixture(t)
	a, b := strings.Repeat("1", 40), strings.Repeat("2", 40)
	seedGreenPromotionCandidate(t, f, a)
	seedGreenPromotionCandidate(t, f, b)
	c := f.api(t)
	c.git = &trainControlGit{controlGit: f.git}
	c.promotionGrants = f.store
	f.git.plan = gitcache.MergeCandidate{BaseSHA: strings.Repeat("b", 40), MergedSHA: strings.Repeat("c", 40)}
	actor := api.Actor{Identity: "dev@host", Scope: []string{"codeberg"}}
	args := json.RawMessage(fmt.Sprintf(`{"branch":"main","sources":[{"sha":%q},{"sha":%q}]}`, a, b))
	for i, sha := range []string{a, b} {
		if _, err := f.store.AppendPromotionGrant(t.Context(), model.PromotionGrantSpec{RepoID: f.repo.ID, SourceSHA: sha, TargetRef: "main", Issuer: "admin", IssuerFingerprint: "SHA256:admin", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
		value, err := c.CallTool(t.Context(), actor, "promote", args)
		if i == 0 {
			if err == nil || !errors.Is(err, ErrForbidden) {
				t.Fatalf("missing second grant: %v", err)
			}
			page, err := f.store.ListPromotions(t.Context(), model.PromotionListFilter{})
			if err != nil || len(page.Promotions) != 0 {
				t.Fatalf("partial admission: %+v %v", page, err)
			}
		} else {
			if err != nil {
				t.Fatal(err)
			}
			p := requireToolPromotion(t, f, value)
			if p.RunID == "" || p.Train.Sources[0].GrantID == "" || p.Train.Sources[1].GrantID == "" {
				t.Fatalf("scoped grant train: %+v", p)
			}
		}
	}
}

// The promote target is a branch name: a full ref or an invalid name is
// refused before anything is admitted or audited.
func TestPromoteRefusesARefOrInvalidTarget(t *testing.T) {
	for target, want := range map[string]string{"refs/heads/main": `name the target branch, e.g. "main"`, "-main": "promotion target"} {
		f := newControlFixture(t)
		sha := strings.Repeat("1", 40)
		seedGreenPromotionCandidate(t, f, sha)
		c := f.api(t)
		body := fmt.Sprintf(`{"repo":%q,"sha":%q,"branch":%q}`, f.repo.Name, sha, target)
		_, err := c.CallTool(t.Context(), api.Actor{Identity: "admin@host", Admin: true}, "promote", json.RawMessage(body))
		if !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), want) {
			t.Fatalf("target %q: %v", target, err)
		}
		page, err := f.store.ListPromotions(t.Context(), model.PromotionListFilter{})
		if err != nil || len(page.Promotions) != 0 {
			t.Fatalf("target %q admitted: %+v %v", target, page, err)
		}
	}
}
