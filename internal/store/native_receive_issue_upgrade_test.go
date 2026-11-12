package store

import (
	"errors"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
)

func TestNativeReceiveUpgradeProjectsFailureAndResolvesEarlierIssueWork(t *testing.T) {
	s := nativeReceiveUpgradeStore(t)
	repo := projectionRepository(t, s)
	ctx := t.Context()
	old := projectionRun(t, s, repo.ID, "main", projectionSourceSHA)
	if _, err := s.FinishRun(ctx, old.ID, model.RunResult{Status: model.RunFailed, Phase: "test"}); err != nil {
		t.Fatal(err)
	}
	promotion, err := s.AppendPromotion(ctx, model.PromotionSpec{RepoID: repo.ID, SourceBranch: "main", SourceSHA: projectionSourceSHA, TargetRef: "main", PreviousSHA: projectionBaseSHA, ResultSHA: projectionResultSHA, Actor: "agent@host"})
	if err != nil {
		t.Fatal(err)
	}
	seedEarlierPromotionIssue(t, s, promotion)
	var previous int64
	if err = s.db.QueryRowContext(ctx, `SELECT max(sequence) FROM ci_issue_work`).Scan(&previous); err != nil {
		t.Fatal(err)
	}
	specs := []model.RunSpec{testRunSpec(repo.ID, "main", projectionBaseSHA), testRunSpec(repo.ID, "main", projectionResultSHA)}
	for i := range specs {
		specs[i].Trigger = "schedule"
		specs[i].ScheduleEntry = []string{"red", "green"}[i]
		seedDeferredReceive(t, s, specs[i].ScheduleEntry, specs[i])
	}
	applyNativeReceiveUpgrade(t, s)
	rows, err := s.db.QueryContext(ctx, `SELECT sequence,run_id,actor,created_at=(SELECT created_at FROM runs WHERE id=run_id) FROM ci_issue_work WHERE sequence>? ORDER BY sequence`, previous)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for rows.Next() {
		var sequence int64
		var id, actor string
		var sameTime bool
		if err = rows.Scan(&sequence, &id, &actor, &sameTime); err != nil {
			t.Fatal(err)
		}
		if sequence != previous+int64(len(ids))+1 || actor != specs[len(ids)].Actor || !sameTime {
			t.Fatal("issue work order/provenance", sequence, actor, sameTime)
		}
		ids = append(ids, id)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	if len(ids) != 2 {
		t.Fatal("missing converted issue work", ids)
	}
	for i, id := range ids {
		run, err := s.ClaimNextRun(ctx)
		if err != nil || run.ID != id {
			t.Fatal(run, err)
		}
		status := model.RunFailed
		if i == 1 {
			status = model.RunPassed
		}
		if _, err = s.FinishRun(ctx, id, model.RunResult{Status: status, Phase: "test"}); err != nil {
			t.Fatal(err)
		}
		issue, err := s.OpenCIIssue(ctx, repo.ID, "main")
		if i == 0 && (err != nil || !strings.Contains(issue.Body, id)) {
			t.Fatal("converted failure did not update issue", issue, err)
		}
		if i == 1 && !errors.Is(err, ErrNotFound) {
			t.Fatal("converted green did not resolve branch/promotion work", issue, err)
		}
	}
	var count int
	if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM ci_issue_work WHERE sequence<=?`, previous).Scan(&count); err != nil || int64(count) != previous {
		t.Fatal("existing immutable work changed", count, err)
	}
}

func TestNativeReceiveUpgradeLateGreenCannotResolveNewerFailure(t *testing.T) {
	s := nativeReceiveUpgradeStore(t)
	repo := projectionRepository(t, s)
	ctx := t.Context()
	spec := testRunSpec(repo.ID, "main", projectionSourceSHA)
	seedDeferredReceive(t, s, "converted-green", spec)
	applyNativeReceiveUpgrade(t, s)
	green, err := s.ClaimNextRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	newerSpec := testRunSpec(repo.ID, "main", projectionResultSHA)
	newerSpec.Trigger = "schedule"
	newerSpec.ScheduleEntry = "nightly"
	newer, err := s.EnqueueRun(ctx, newerSpec)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := s.ClaimNextRun(ctx)
	if err != nil || claimed.ID != newer.ID {
		t.Fatal(claimed, err)
	}
	if _, err = s.FinishRun(ctx, newer.ID, model.RunResult{Status: model.RunFailed, Phase: "test"}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.FinishRun(ctx, green.ID, model.RunResult{Status: model.RunPassed, Phase: "test"}); err != nil {
		t.Fatal(err)
	}
	issue, err := s.OpenCIIssue(ctx, repo.ID, "main")
	if err != nil || !strings.Contains(issue.Body, newer.ID) {
		t.Fatal("late converted green hid newer work", issue, err)
	}
}

func TestNativeReceiveUpgradeIssueWorkMatchesEnqueuePolicy(t *testing.T) {
	for _, tc := range []struct {
		ref, trigger string
		kind         model.RefKind
		want         int
	}{
		{"main", "branch", model.RefBranch, 1}, {"feature", "schedule", model.RefBranch, 1},
		{"feature", "branch", model.RefBranch, 0}, {"main", "promotion", model.RefBranch, 0},
		{"main", "plan", model.RefBranch, 0}, {"main", "apply", model.RefBranch, 0}, {"v1.0.0", "tag", model.RefTag, 0},
	} {
		t.Run(tc.ref+"-"+tc.trigger, func(t *testing.T) {
			s := nativeReceiveUpgradeStore(t)
			repo := projectionRepository(t, s)
			spec := testRunSpec(repo.ID, tc.ref, projectionSourceSHA)
			spec.Trigger = tc.trigger
			spec.RefKind = tc.kind
			seedDeferredReceive(t, s, "receipt", spec)
			applyNativeReceiveUpgrade(t, s)
			var count int
			if err := s.db.QueryRowContext(t.Context(), `SELECT count(*) FROM ci_issue_work`).Scan(&count); err != nil || count != tc.want {
				t.Fatal("issue work eligibility", count, err)
			}
		})
	}
}
