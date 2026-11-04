package store

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/testdb"
)

func TestParkedPromotionEvidenceIsDurableAndImmutable(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	ctx := t.Context()
	up, err := s.CreateUpstream(ctx, model.UpstreamSpec{Name: "forge", Kind: "ssh", BaseURL: "ssh://git.example/org"})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := s.CreateRepository(ctx, model.RepositorySpec{Name: "repo", UpstreamID: up.ID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	sha, base := strings.Repeat("a", 40), strings.Repeat("b", 40)
	p, err := s.AppendPromotion(ctx, model.PromotionSpec{RepoID: repo.ID, SourceBranch: "park/review", SourceSHA: sha, TargetRef: "main", Actor: "admin", BranchCISkipped: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(ctx, `UPDATE promotions SET branch_ci_skipped=0 WHERE id=?`, p.ID); err == nil {
		t.Fatal("branch CI evidence was mutable")
	}
	p, err = s.PlanPromotion(ctx, p.ID, base, sha)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.BeginPublication(ctx, model.PublicationSpec{RepoID: repo.ID, PromotionID: p.ID, RefKind: model.RefBranch, Ref: "main", PreviousSHA: base, PreviousKnown: true, ResultSHA: sha, Actor: "admin"}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("untested park publication=%v", err)
	}
	queued, p, err := s.EnqueueAdmittedPromotionRun(ctx, model.RunSpec{RepoID: repo.ID, RefKind: model.RefBranch, Ref: "promotion/main/aaaa", SHA: sha, TestedSHA: sha, BaseSHA: base, Actor: "admin", Trigger: "promotion"}, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if queued.Reason != "promoted without branch CI" {
		t.Fatalf("reason=%q", queued.Reason)
	}
	for _, read := range []func() (model.Promotion, error){func() (model.Promotion, error) { return s.Promotion(ctx, p.ID) }, func() (model.Promotion, error) { return s.PromotionByRun(ctx, queued.ID) }} {
		found, err := read()
		if err != nil || !found.BranchCISkipped {
			t.Fatalf("lost evidence=%+v %v", found, err)
		}
	}
	page, err := s.ListPromotions(ctx, model.PromotionListFilter{RepoID: repo.ID})
	if err != nil || len(page.Promotions) != 1 || !page.Promotions[0].BranchCISkipped {
		t.Fatalf("page=%+v %v", page, err)
	}
	if _, err = s.FinishPromotion(ctx, p.ID, model.PromotionFailed, p.RunID, "red"); err != nil {
		t.Fatal(err)
	}
	found, err := s.Promotion(ctx, p.ID)
	if err != nil || !found.BranchCISkipped {
		t.Fatalf("terminal lost evidence=%+v %v", found, err)
	}
	actions, _, err := s.ListAuditActions(ctx, model.AuditListFilter{ActionPrefix: "promotion.admit"})
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 {
		t.Fatalf("actions=%+v", actions)
	}
	var fields map[string]string
	if err = json.Unmarshal([]byte(actions[0].Details), &fields); err != nil {
		t.Fatal(err)
	}
	if fields["branch_ci_skipped"] != "true" {
		t.Fatalf("audit=%v", fields)
	}
	// Parked receive receipts are replay-safe and never enqueue a run.
	event := model.ReceiveEventSpec{ID: "park-receive", Actor: "agent", RepoID: repo.ID, RefKind: model.RefBranch, Ref: "park/review", ObjectSHA: sha, CommitSHA: sha, Outcome: "parked"}
	if duplicate, err := s.RecordReceiveEvent(ctx, event); err != nil || duplicate {
		t.Fatalf("first receipt=%v %v", duplicate, err)
	}
	if duplicate, err := s.RecordReceiveEvent(ctx, event); err != nil || !duplicate {
		t.Fatalf("replay=%v %v", duplicate, err)
	}
}

func TestParkPromotionUpgradeFromV53PreservesHistory(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "upgrade.sqlite")
	s, err := OpenAdminClient(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	repo := createRepo(t, s)
	p, err := s.AppendPromotion(ctx, model.PromotionSpec{RepoID: repo.ID, SourceBranch: "feature", SourceSHA: strings.Repeat("a", 40), TargetRef: "main", Actor: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(ctx, testdb.PostV53Downgrade); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		s, err = OpenAdminClient(ctx, path, Options{})
		if err != nil {
			t.Fatal(err)
		}
		got, readErr := s.Promotion(ctx, p.ID)
		if readErr != nil || got.BranchCISkipped || got.SourceSHA != p.SourceSHA {
			t.Fatalf("historical promotion=%+v %v", got, readErr)
		}
		if err = s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
