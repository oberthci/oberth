package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/testdb"
)

type trainPlanner interface {
	PlanPromotionTrain(context.Context, string, string, string, *model.PromotionTrain) (model.Promotion, error)
}

func TestPromotionTrainRecordsPlanAndAllGrantAuthority(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	sources := []model.PromotionSource{{SHA: strings.Repeat("a", 40), Ref: "feature/a"}, {SHA: strings.Repeat("b", 40), Ref: "feature/b"}}
	for i := range sources {
		grant, err := s.AppendPromotionGrant(t.Context(), model.PromotionGrantSpec{RepoID: repo.ID, SourceSHA: sources[i].SHA, TargetRef: "main", Issuer: "admin", IssuerFingerprint: "SHA256:admin", ExpiresAt: now.Add(time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		sources[i].GrantID = grant.ID
	}
	spec := model.PromotionSpec{RepoID: repo.ID, SourceBranch: sources[0].Ref, SourceSHA: sources[0].SHA, TargetRef: "main", Actor: "developer", GrantID: sources[0].GrantID, Train: &model.PromotionTrain{Sources: sources, SkipConflicting: true}}
	p, err := s.AppendPromotion(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := s.Promotion(t.Context(), p.ID)
	if err != nil || loaded.Train == nil || len(loaded.Train.Sources) != 2 {
		t.Fatalf("train lost: %+v %v", loaded, err)
	}
	for _, source := range sources {
		if _, err := s.MatchingPromotionGrant(t.Context(), repo.ID, source.SHA, "main"); !errors.Is(err, ErrInvalidState) {
			t.Errorf("grant not reserved for %s: %v", source.SHA, err)
		}
		single := model.PromotionSpec{RepoID: repo.ID, SourceBranch: source.Ref, SourceSHA: source.SHA, TargetRef: "main", Actor: "developer", GrantID: source.GrantID}
		if _, err := s.AppendPromotion(t.Context(), single); !errors.Is(err, ErrInvalidState) {
			t.Errorf("second holder admitted: %v", err)
		}
	}
	planner, ok := any(s).(trainPlanner)
	if !ok {
		t.Fatal("missing atomic train planner")
	}
	resolved := &model.PromotionTrain{Sources: append([]model.PromotionSource(nil), sources...), SkipConflicting: true, TreeSHA: strings.Repeat("e", 40)}
	resolved.Sources[1].Skipped = true
	resolved.Sources[1].ConflictPaths = []string{"file.txt"}
	p, err = planner.PlanPromotionTrain(t.Context(), p.ID, strings.Repeat("c", 40), strings.Repeat("d", 40), resolved)
	if err != nil {
		t.Fatal(err)
	}
	if p.Train.TreeSHA != resolved.TreeSHA || !strings.Contains(p.Error, sources[1].SHA) || !strings.Contains(p.Error, "file.txt") {
		t.Fatalf("missing skip result: %+v", p)
	}
	action, err := s.LatestAuditActionByType(t.Context(), "promotion.pending", "promotion")
	if err != nil {
		t.Fatal(err)
	}
	var details map[string]any
	if err := json.Unmarshal([]byte(action.Details), &details); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(action.Details, sources[0].SHA) || !strings.Contains(action.Details, sources[1].SHA) || !strings.Contains(action.Details, resolved.TreeSHA) {
		t.Fatalf("incomplete audit: %s", action.Details)
	}
	if _, err := planner.PlanPromotionTrain(t.Context(), p.ID, p.PreviousSHA, p.ResultSHA, resolved); err != nil {
		t.Fatalf("idempotent plan: %v", err)
	}
	changed := *resolved
	changed.Sources = append([]model.PromotionSource(nil), resolved.Sources...)
	changed.Sources[0], changed.Sources[1] = changed.Sources[1], changed.Sources[0]
	if _, err := planner.PlanPromotionTrain(t.Context(), p.ID, p.PreviousSHA, p.ResultSHA, &changed); err == nil {
		t.Fatal("rewrote source order")
	}
	tx, err := s.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := checkPromotionDeliveryGrantTx(t.Context(), tx, p, unixNano(now)); err != nil {
		t.Fatal(err)
	}
	if err := s.consumeDeliveredPromotionGrantTx(t.Context(), tx, p, "publication", unixNano(now)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for i, source := range sources {
		grant, err := s.PromotionGrant(t.Context(), source.GrantID)
		if err != nil {
			t.Fatal(err)
		}
		if (grant.ConsumedAt != nil) != (i == 0) {
			t.Fatalf("wrong grant consumption: %+v", grant)
		}
	}
}

func TestPromotionTrainUpgradeFrom52PreservesSingleSource(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	p, err := s.AppendPromotion(t.Context(), model.PromotionSpec{RepoID: repo.ID, SourceSHA: strings.Repeat("a", 40), SourceBranch: "feature/a", TargetRef: "main", Actor: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(testdb.PostV52Downgrade); err != nil {
		t.Fatal(err)
	}
	path := s.livePath
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenAdminClient(t.Context(), path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	got, err := s.Promotion(t.Context(), p.ID)
	if err != nil || !reflect.DeepEqual(p, got) {
		t.Fatalf("legacy changed: %+v %v", got, err)
	}
	if err = s.migrate(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err = s.VerifyAuditState(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestPromotionTrainChecksEveryGrantAtDelivery(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	sources := []model.PromotionSource{{SHA: strings.Repeat("a", 40), Ref: "feature/a"}, {SHA: strings.Repeat("b", 40), Ref: "feature/b"}}
	for i := range sources {
		g, err := s.AppendPromotionGrant(t.Context(), model.PromotionGrantSpec{RepoID: repo.ID, SourceSHA: sources[i].SHA, TargetRef: "main", Issuer: "admin", IssuerFingerprint: "SHA256:admin", ExpiresAt: now.Add(time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		sources[i].GrantID = g.ID
	}
	p, err := s.AppendPromotion(t.Context(), model.PromotionSpec{RepoID: repo.ID, SourceBranch: sources[0].Ref, SourceSHA: sources[0].SHA, TargetRef: "main", Actor: "dev", GrantID: sources[0].GrantID, Train: &model.PromotionTrain{Sources: sources}})
	if err != nil {
		t.Fatal(err)
	}
	resolved := &model.PromotionTrain{Sources: sources, TreeSHA: strings.Repeat("f", 40)}
	p, err = s.PlanPromotionTrain(t.Context(), p.ID, strings.Repeat("c", 40), strings.Repeat("d", 40), resolved)
	if err != nil {
		t.Fatal(err)
	}
	// A train can never take the single-source fast-forward publication path.
	if _, err = s.BeginPublication(t.Context(), model.PublicationSpec{RepoID: repo.ID, PromotionID: p.ID, RefKind: model.RefBranch, Ref: "main", PreviousSHA: p.PreviousSHA, PreviousKnown: true, ResultSHA: p.ResultSHA, Actor: "dev"}); err == nil {
		t.Fatal("train published without CI")
	}
	if _, err = s.RevokePromotionGrant(t.Context(), sources[1].GrantID, "admin"); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if err = checkPromotionDeliveryGrantTx(t.Context(), tx, p, unixNano(now)); err == nil {
		t.Fatal("second grant revocation ignored")
	}
}
