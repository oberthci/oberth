package store

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/testdb"
)

func TestPromotionConfirmationMigrationPreservesHistory(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	p, err := s.AppendPromotion(t.Context(), model.PromotionSpec{RepoID: repo.ID, SourceBranch: "feature/confirm", SourceSHA: strings.Repeat("b", 40), TargetRef: "main", PreviousSHA: strings.Repeat("a", 40), ResultSHA: strings.Repeat("b", 40), Actor: "developer"})
	if err != nil {
		t.Fatal(err)
	}
	pub, err := s.BeginPublication(t.Context(), model.PublicationSpec{RepoID: repo.ID, PromotionID: p.ID, RefKind: model.RefBranch, Ref: "main", PreviousSHA: p.PreviousSHA, ResultSHA: p.ResultSHA, Actor: p.Actor})
	if err != nil {
		t.Fatal(err)
	}
	final, err := s.FinalizePublication(t.Context(), pub.ID, model.PublicationDelivered, "")
	if err != nil || final.Promotion.ObservedTargetSHA != p.ResultSHA {
		t.Fatalf("final=%+v err=%v", final, err)
	}
	if _, err = s.db.Exec(`UPDATE promotions SET observed_target_sha='' WHERE id=?`, p.ID); err == nil {
		t.Fatal("terminal observation was mutable")
	}
	before, err := s.VerifyAuditChain(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// Reconstruct the previous schema without modifying any historical audit row.
	if _, err = s.db.Exec(testdb.PostPromotionConfirmationDowngrade); err != nil {
		t.Fatal(err)
	}

	if err = s.migrate(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	after, err := s.VerifyAuditChain(t.Context())
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("audit history changed: before=%+v after=%+v err=%v", before, after, err)
	}
	loaded, err := s.Promotion(t.Context(), p.ID)
	if err != nil || loaded.Status != model.PromotionPassed || loaded.ResultSHA != p.ResultSHA || loaded.ObservedTargetSHA != "" {
		t.Fatalf("historical promotion=%+v err=%v", loaded, err)
	}
	if _, err = s.db.Exec(`UPDATE promotions SET observed_target_sha=result_sha WHERE id=?`, p.ID); err == nil {
		t.Fatal("historical confirmation fabricated")
	}
}
