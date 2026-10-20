package service

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

type releaseCheckGit struct {
	*controlGit
	source, base []byte
}

func (g *releaseCheckGit) ReadOptionalBlob(_ context.Context, _ string, sha, file string, _ int) ([]byte, bool, error) {
	if file != releasePreflightFile {
		return nil, false, nil
	}
	if sha == strings.Repeat("b", 40) {
		return g.base, g.base != nil, nil
	}
	return g.source, g.source != nil, nil
}
func (g *releaseCheckGit) ReadDefaultBlob(context.Context, string, string, int) (string, []byte, bool, error) {
	return "", g.base, g.base != nil, nil
}

type releaseChecker struct {
	calls    int
	problems []string
}

func (c *releaseChecker) PreflightRelease(context.Context, JobRequest, []byte) ([]string, error) {
	c.calls++
	return c.problems, nil
}

type releaseCheckJobs struct {
	JobController
	*releaseChecker
}

func TestReleasePreflightBranchWarningAndPromotionRefusal(t *testing.T) {
	checker := &releaseChecker{problems: []string{"access_allow exact-call"}}
	git := &releaseCheckGit{source: []byte("new"), base: []byte("old")}
	scheduler := &Scheduler{git: git, jobs: &releaseCheckJobs{releaseChecker: checker}}
	request := JobRequest{Run: model.Run{SHA: strings.Repeat("a", 40), Trigger: "branch"}}
	var output bytes.Buffer
	if err := scheduler.checkReleasePreflight(context.Background(), request, &output); err != nil {
		t.Fatal(err)
	}
	if checker.calls != 1 || !strings.Contains(output.String(), "[release-preflight/check] warning: access_allow") {
		t.Fatalf("%d %s", checker.calls, output.String())
	}
	request.Run.Trigger = "promotion"
	if err := scheduler.checkReleasePreflight(context.Background(), request, &output); err == nil {
		t.Fatal("promotion accepted grant gap")
	}
	git.base = git.source
	checker.calls = 0
	if err := scheduler.checkReleasePreflight(context.Background(), request, &output); err != nil || checker.calls != 0 {
		t.Fatalf("unchanged document checked: %v calls=%d", err, checker.calls)
	}
}

func TestReleasePreflightBlocksFastForwardBeforePublication(t *testing.T) {
	fixture := newControlFixture(t)
	sha := strings.Repeat("a", 40)
	seedGreenPromotionCandidate(t, fixture, sha)
	fixture.git.plan = gitcache.MergeCandidate{BaseSHA: strings.Repeat("b", 40), MergedSHA: sha, FastForward: true}
	service := fixture.api(t)
	checker := &releaseChecker{problems: []string{"access_allow exact-call"}}
	service.releasePreflight = checker
	service.git = &releaseCheckGit{controlGit: fixture.git, source: []byte("new"), base: []byte("old")}
	value, err := service.CallTool(context.Background(), api.Actor{Identity: "admin", Admin: true}, "promote", []byte(`{"repo":"oberth","sha":"`+sha+`","branch":"main"}`))
	if err != nil {
		t.Fatal(err)
	}
	promotion := requireToolPromotion(t, fixture, value)
	if promotion.Status != model.PromotionFailed || !strings.Contains(promotion.Error, "access_allow") || len(fixture.git.promotions) != 0 {
		t.Fatalf("promotion=%+v pushed=%v", promotion, fixture.git.promotions)
	}
}

type grantRefusingReleaseJobs struct {
	*fakeJobs
	database *store.Store
}

func (jobs *grantRefusingReleaseJobs) CreateRelease(ctx context.Context, request JobRequest) error {
	grants, err := jobs.database.ActiveSecretGrants(ctx, request.Repository.ID)
	if err != nil {
		return err
	}
	if !grants["publish"]["oberth/data/release/signing"] {
		return errors.New("secret grant denied: publish lacks signing grant")
	}
	return jobs.fakeJobs.CreateRelease(ctx, request)
}

func TestTagRerunAfterReleaseGrantAdmissionRefusal(t *testing.T) {
	harness := newRerunHarness(t, greenResult)
	harness.scheduler.releaseJobs = &grantRefusingReleaseJobs{fakeJobs: harness.fixture.jobs, database: harness.fixture.store}
	failed := admitRerunTag(t, harness)
	if failed.Status != model.RunFailed || !strings.Contains(failed.Error, "secret grant denied") || len(harness.git.syncedTags) != 0 {
		t.Fatalf("admission refusal=%+v", failed)
	}
	if _, err := harness.fixture.store.Grant(context.Background(), "codeberg/acme/oberth", "publish", "oberth/data/release/signing", "admin"); err != nil {
		t.Fatal(err)
	}
	response := harness.rerun(t, rerunAdmin, map[string]any{"id": failed.ID, "force": true, "reason": "release grant approved"})
	harness.process(t)
	passed := harness.run(t, response.RunID)
	if passed.Status != model.RunPassed || passed.SHA != failed.SHA || passed.TestedSHA != failed.TestedSHA || len(harness.git.syncedTags) != 1 {
		t.Fatalf("rerun=%+v publications=%v", passed, harness.git.syncedTags)
	}
}
