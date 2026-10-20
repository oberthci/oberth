package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

const (
	retryStoreSource = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	retryStoreBase   = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	retryStoreMerged = "cccccccccccccccccccccccccccccccccccccccc"
)

func retryPromotionSpec(repoID int64, grantID string) model.PromotionSpec {
	return model.PromotionSpec{RepoID: repoID, SourceBranch: "feature/retry", SourceSHA: retryStoreSource,
		TargetRef: "main", Actor: "agent@host", GrantID: grantID}
}

// failMergedPromotion drives one admitted promotion through planning, its
// merged CI run and a failed publication, the path #1097's audit exercised.
func failMergedPromotion(t *testing.T, s *Store, now *time.Time, spec model.PromotionSpec) (model.Promotion, model.Publication) {
	t.Helper()
	ctx := context.Background()
	promotion, err := s.AppendPromotion(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PlanPromotion(ctx, promotion.ID, retryStoreBase, retryStoreMerged); err != nil {
		t.Fatal(err)
	}
	enqueued, promotion, err := s.EnqueueAdmittedPromotionRun(ctx, model.RunSpec{
		RepoID: spec.RepoID, RefKind: model.RefBranch, Ref: "promotion/main/" + retryStoreMerged[:12],
		SHA: retryStoreMerged, Actor: spec.Actor, Trigger: "promotion", TestedSHA: retryStoreMerged, BaseSHA: retryStoreBase,
	}, promotion.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	publication, err := s.BeginPublication(ctx, model.PublicationSpec{
		RepoID: spec.RepoID, RunID: enqueued.ID, PromotionID: promotion.ID, RefKind: model.RefBranch, Ref: "main",
		PreviousSHA: retryStoreBase, PreviousKnown: true, ResultSHA: retryStoreMerged, Actor: spec.Actor,
	})
	if err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Second)
	finalization, err := s.FinalizePublication(ctx, publication.ID, model.PublicationFailed, "ssh: connection reset by peer")
	if err != nil {
		t.Fatal(err)
	}
	return finalization.Promotion, finalization.Publication
}

// publish_retry used to try failed->pending on the immutable promotion row
// and always hit promotions_guard_update (#1097). It now refuses a promotion
// publication by either ID form, before any state changes.
func TestRetryFailedPublicationRefusesPromotionPublicationWithoutChangingState(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	promotion, publication := failMergedPromotion(t, s, &now, retryPromotionSpec(repo.ID, ""))
	head, err := s.TailAuditAction(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, ids := range [][2]string{{promotion.RunID, ""}, {"", promotion.ID}} {
		if _, err := s.RetryFailedPublication(ctx, ids[0], ids[1], "retry@host"); !errors.Is(err, ErrInvalidState) {
			t.Fatalf("RetryFailedPublication(%q, %q) error = %v, want ErrInvalidState", ids[0], ids[1], err)
		}
	}
	after, err := s.Promotion(ctx, promotion.ID)
	if err != nil || after.Status != model.PromotionFailed {
		t.Fatalf("promotion after refused retry = %#v, %v", after, err)
	}
	run, err := s.Run(ctx, promotion.RunID)
	if err != nil || run.Status != model.RunFailed || run.Phase != "publishing" {
		t.Fatalf("promotion run after refused retry = %#v, %v", run, err)
	}
	if again, err := s.Publication(ctx, publication.ID); err != nil || again.Status != model.PublicationFailed {
		t.Fatalf("publication after refused retry = %#v, %v", again, err)
	}
	if tail, err := s.TailAuditAction(ctx); err != nil || tail.ID != head.ID {
		t.Fatalf("audit tail moved from %d to %#v (%v) on a refused retry", head.ID, tail, err)
	}
}

func TestPromotionRetryAdmissionLineageAndGuards(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	spec := retryPromotionSpec(repo.ID, "")

	pending, err := s.AppendPromotion(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	retry := spec
	retry.RetryOf, retry.RetryMode = pending.ID, model.PromotionRetryManual
	if _, err := s.AppendPromotion(ctx, retry); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("retry of a pending promotion error = %v, want ErrInvalidState", err)
	}
	if _, err := s.FinishPromotion(ctx, pending.ID, model.PromotionInterrupted, "", "server restarted"); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]model.PromotionSpec{
		"no mode":      {RepoID: repo.ID, SourceBranch: "feature/retry", SourceSHA: retryStoreSource, TargetRef: "main", Actor: "agent@host", RetryOf: pending.ID},
		"other target": {RepoID: repo.ID, SourceBranch: "feature/retry", SourceSHA: retryStoreSource, TargetRef: "release", Actor: "agent@host", RetryOf: pending.ID, RetryMode: model.PromotionRetryManual},
	} {
		if _, err := s.AppendPromotion(ctx, bad); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s retry error = %v, want ErrInvalid", name, err)
		}
	}

	retry.RetryMode = model.PromotionRetryAuto
	first, err := s.AppendPromotion(ctx, retry)
	if err != nil {
		t.Fatalf("retry of an interrupted promotion: %v", err)
	}
	if _, err := s.AppendPromotion(ctx, retry); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("second retry of the same promotion error = %v, want ErrInvalidState", err)
	}
	if _, err := s.FinishPromotion(ctx, first.ID, model.PromotionFailed, "", "upstream main moved"); err != nil {
		t.Fatal(err)
	}
	retry.RetryOf = first.ID
	second, err := s.AppendPromotion(ctx, retry)
	if err != nil {
		t.Fatal(err)
	}
	lineage, err := s.PromotionLineage(ctx, second.ID)
	if err != nil || lineage.RetryOf != first.ID || lineage.Mode != model.PromotionRetryAuto || lineage.AutoAttempt != 2 {
		t.Fatalf("second retry lineage = %#v, %v, want auto attempt 2 of %s", lineage, err, first.ID)
	}
	if original, err := s.PromotionLineage(ctx, pending.ID); err != nil || original.RetriedAs != first.ID || original.RetryOf != "" {
		t.Fatalf("original lineage = %#v, %v", original, err)
	}
	// A manual retry restarts the automatic count.
	if _, err := s.FinishPromotion(ctx, second.ID, model.PromotionFailed, "", "upstream main moved"); err != nil {
		t.Fatal(err)
	}
	retry.RetryOf, retry.RetryMode = second.ID, model.PromotionRetryManual
	manual, err := s.AppendPromotion(ctx, retry)
	if err != nil {
		t.Fatal(err)
	}
	if lineage, _ := s.PromotionLineage(ctx, manual.ID); lineage.AutoAttempt != 0 || lineage.Mode != model.PromotionRetryManual {
		t.Fatalf("manual retry lineage = %#v", lineage)
	}
	// While the manual retry is pending, an older failed link cannot fork
	// a second pending promotion of the tuple.
	retry.RetryOf = first.ID
	if _, err := s.AppendPromotion(ctx, retry); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("forked retry error = %v, want ErrInvalidState", err)
	}
	if _, err := s.VerifyAuditState(ctx); err != nil {
		t.Fatalf("audit chain after retries: %v", err)
	}
}

// A grant is held by its promotion until that promotion terminalizes and is
// consumed only by a delivered publication (#1097).
func TestPromotionGrantIsHeldAtAdmissionAndConsumedOnDelivery(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	grant, err := s.AppendPromotionGrant(ctx, model.PromotionGrantSpec{
		RepoID: repo.ID, SourceSHA: retryStoreSource, TargetRef: "main",
		Issuer: "admin@host", IssuerFingerprint: "SHA256:admin", ExpiresAt: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	spec := retryPromotionSpec(repo.ID, grant.ID)

	failed, _ := failMergedPromotion(t, s, &now, spec)
	if held, err := s.PromotionGrant(ctx, grant.ID); err != nil || held.ConsumedAt != nil {
		t.Fatalf("grant after a failed delivery = %#v, %v, want unconsumed", held, err)
	}
	if match, err := s.MatchingPromotionGrant(ctx, repo.ID, retryStoreSource, "main"); err != nil || match.ID != grant.ID {
		t.Fatalf("grant after a failed delivery does not match again: %#v, %v", match, err)
	}

	retry := spec
	retry.RetryOf, retry.RetryMode = failed.ID, model.PromotionRetryManual
	holder, err := s.AppendPromotion(ctx, retry)
	if err != nil {
		t.Fatalf("retry under the released grant: %v", err)
	}
	if _, err := s.MatchingPromotionGrant(ctx, repo.ID, retryStoreSource, "main"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("matching a held grant error = %v, want ErrInvalidState naming the holder", err)
	}
	if _, err := s.AppendPromotion(ctx, spec); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("second promotion under a held grant error = %v, want ErrInvalidState", err)
	}

	if _, err := s.PlanPromotion(ctx, holder.ID, retryStoreBase, retryStoreSource); err != nil {
		t.Fatal(err)
	}
	publication, err := s.BeginPublication(ctx, model.PublicationSpec{
		RepoID: repo.ID, PromotionID: holder.ID, RefKind: model.RefBranch, Ref: "main",
		PreviousSHA: retryStoreBase, PreviousKnown: true, ResultSHA: retryStoreSource, Actor: spec.Actor,
	})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	if _, err := s.FinalizePublication(ctx, publication.ID, model.PublicationDelivered, ""); err != nil {
		t.Fatal(err)
	}
	consumed, err := s.PromotionGrant(ctx, grant.ID)
	if err != nil || consumed.ConsumedAt == nil || consumed.ConsumedBy != holder.ID {
		t.Fatalf("grant after delivery = %#v, %v, want consumed by %s", consumed, err, holder.ID)
	}
	if action, err := s.LatestAuditActionByType(ctx, "promotion_grant.consume", "promotion_grant"); err != nil || action.ResourceID != grant.ID {
		t.Fatalf("consumption audit = %#v, %v", action, err)
	}
	if _, err := s.AppendPromotion(ctx, spec); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("promotion under a consumed grant error = %v, want ErrInvalidState", err)
	}
	if _, err := s.VerifyAuditState(ctx); err != nil {
		t.Fatalf("audit chain after grant lifecycle: %v", err)
	}
}

// A revoked or expired grant is not reusable by a retry.
func TestReleasedPromotionGrantStillHonoursRevocationAndExpiry(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	grant, err := s.AppendPromotionGrant(ctx, model.PromotionGrantSpec{
		RepoID: repo.ID, SourceSHA: retryStoreSource, TargetRef: "main",
		Issuer: "admin@host", IssuerFingerprint: "SHA256:admin", ExpiresAt: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	spec := retryPromotionSpec(repo.ID, grant.ID)
	failed, _ := failMergedPromotion(t, s, &now, spec)
	retry := spec
	retry.RetryOf, retry.RetryMode = failed.ID, model.PromotionRetryManual

	later := now.Add(2 * time.Hour)
	expiredStore := now
	now = later
	if _, err := s.AppendPromotion(ctx, retry); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("retry under an expired grant error = %v, want ErrInvalidState", err)
	}
	now = expiredStore
	if _, err := s.RevokePromotionGrant(ctx, grant.ID, "admin@host"); err != nil {
		t.Fatalf("revoking a released grant: %v", err)
	}
	if _, err := s.AppendPromotion(ctx, retry); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("retry under a revoked grant error = %v, want ErrInvalidState", err)
	}
}

func TestUplinksByIdentityReportsCurrentAuthority(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	register := func(fingerprint string, digestByte byte, admin bool, scope ...string) {
		t.Helper()
		digest := make([]byte, 32)
		digest[0] = digestByte
		credential, err := s.CreateTokenCredential(ctx, model.TokenCredentialSpec{Name: "agent@host", Digest: digest})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.RegisterUplink(ctx, "admin@host", model.UplinkSpec{Fingerprint: fingerprint, Identity: "agent@host",
			TokenCredentialID: credential.ID, Admin: admin, Scope: scope}); err != nil {
			t.Fatal(err)
		}
	}
	if uplinks, err := s.UplinksByIdentity(ctx, "agent@host"); err != nil || len(uplinks) != 0 {
		t.Fatalf("unknown identity = %#v, %v", uplinks, err)
	}
	register("SHA256:laptop", 1, true)
	register("SHA256:desktop", 2, false, "github")
	uplinks, err := s.UplinksByIdentity(ctx, "agent@host")
	if err != nil || len(uplinks) != 2 {
		t.Fatalf("uplinks = %#v, %v", uplinks, err)
	}
	if !uplinks[0].Admin || uplinks[1].Admin || len(uplinks[1].Scope) != 1 || uplinks[1].Scope[0] != "github" {
		t.Fatalf("uplink authority = %#v", uplinks)
	}
	if _, err := s.UplinksByIdentity(ctx, " "); !errors.Is(err, ErrInvalid) {
		t.Fatalf("blank identity error = %v", err)
	}
}
