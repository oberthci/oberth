package store

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestAdmissionHoldPersistenceExpiryAndQueueOrder(t *testing.T) {
	now := time.Now().UTC()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ordinary, err := s.EnqueueRun(t.Context(), testRunSpec(repo.ID, "feature/held", strings.Repeat("a", 40)))
	if err != nil {
		t.Fatal(err)
	}
	promotion, err := s.EnqueueRun(t.Context(), model.RunSpec{RepoID: repo.ID, RefKind: model.RefBranch, Ref: "promotion/held", SHA: strings.Repeat("b", 40), Actor: "operator", Trigger: "promotion"})
	if err != nil {
		t.Fatal(err)
	}
	hold, err := s.SetAdmissionHold(t.Context(), "operator", model.AdmissionHoldRequest{On: true, Reason: "release qualification"})
	if err != nil || !hold.ExpiresAt.Equal(now.Add(2*time.Hour)) {
		t.Fatal(hold, err)
	}
	if _, err = s.ClaimNextRun(t.Context()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("held claim: %v", err)
	}
	if _, err = s.NextQueuedRun(t.Context()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("held next: %v", err)
	}
	path := s.livePath
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenAdminClient(t.Context(), path, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	persisted, err := s.AdmissionHold(t.Context())
	if err != nil || persisted.Reason != hold.Reason || !persisted.ExpiresAt.Equal(hold.ExpiresAt) {
		t.Fatal(persisted, err)
	}
	now = hold.ExpiresAt
	next, err := s.ClaimNextRun(t.Context())
	if err != nil || next.ID != ordinary.ID || next.QueueSequence != ordinary.QueueSequence || next.AdmissionSequence != ordinary.AdmissionSequence {
		t.Fatalf("expiry did not resume priority order: %+v %v", next, err)
	}
	next, err = s.ClaimNextRun(t.Context())
	if err != nil || next.ID != promotion.ID {
		t.Fatalf("held work moved: %+v %v", next, err)
	}
	action, err := s.LatestAuditActionByType(t.Context(), "admission.hold", "admission")
	if err != nil || action.Actor != "operator" || !strings.Contains(action.Details, hold.ExpiresAt.Format(time.RFC3339Nano)) {
		t.Fatal(action, err)
	}
}

func TestAdmissionHoldValidationAndAuditRollback(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	for _, request := range []model.AdmissionHoldRequest{
		{On: true}, {On: true, Reason: "x", Duration: 13 * time.Hour}, {On: true, Reason: "x", Duration: -time.Second}, {On: true, Reason: "x", Allow: []string{"branch"}}, {On: true, Reason: "x", Allow: []string{"tag", "tag"}}, {Reason: "off", Duration: time.Hour},
	} {
		if _, err := s.SetAdmissionHold(t.Context(), "admin", request); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid accepted: %+v %v", request, err)
		}
	}
	if _, err := s.db.Exec(`CREATE TRIGGER fail_hold BEFORE INSERT ON audit_actions WHEN NEW.action='admission.hold' BEGIN SELECT RAISE(ABORT,'audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetAdmissionHold(t.Context(), "admin", model.AdmissionHoldRequest{On: true, Reason: "test"}); err == nil {
		t.Fatal("unaudited hold accepted")
	}
	hold, err := s.AdmissionHold(t.Context())
	if err != nil || hold.On {
		t.Fatal("hold survived audit rollback", hold, err)
	}
}

func TestAdmissionHoldClaimsOnlyAllowedClasses(t *testing.T) {
	for _, class := range []string{"tag", "release", "promotion"} {
		t.Run(class, func(t *testing.T) {
			now := time.Now().UTC()
			s := testStore(t, &now)
			repo := createRepo(t, s)
			ordinary, err := s.EnqueueRun(t.Context(), testRunSpec(repo.ID, "feature/ordinary", strings.Repeat("a", 40)))
			if err != nil {
				t.Fatal(err)
			}
			spec := testRunSpec(repo.ID, "feature/allowed", strings.Repeat("b", 40))
			switch class {
			case "tag":
				spec.RefKind = model.RefTag
			case "release":
				spec.Release = true
			case "promotion":
				spec.Trigger = "promotion"
			}
			allowed, err := s.EnqueueRun(t.Context(), spec)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.SetAdmissionHold(t.Context(), "admin", model.AdmissionHoldRequest{On: true, Reason: "selected class", Allow: []string{class}}); err != nil {
				t.Fatal(err)
			}
			next, err := s.NextQueuedRun(t.Context())
			if err != nil || next.ID != allowed.ID {
				t.Fatal(next, err)
			}
			next, err = s.ClaimNextRun(t.Context())
			if err != nil || next.ID != allowed.ID {
				t.Fatal(next, err)
			}
			if _, err = s.ClaimNextRun(t.Context()); !errors.Is(err, ErrNotFound) {
				t.Fatal("ordinary run escaped", err)
			}
			preserved, err := s.Run(t.Context(), ordinary.ID)
			if err != nil || preserved.Status != model.RunQueued || preserved.QueueSequence != ordinary.QueueSequence || preserved.AdmissionSequence != ordinary.AdmissionSequence {
				t.Fatal(preserved, err)
			}
			if _, err = s.SetAdmissionHold(t.Context(), "admin", model.AdmissionHoldRequest{Reason: "finished"}); err != nil {
				t.Fatal(err)
			}
			next, err = s.ClaimNextRun(t.Context())
			if err != nil || next.ID != ordinary.ID {
				t.Fatal(next, err)
			}
		})
	}
}

func TestAdmissionHoldUpgradePreservesPrerequisiteReceipts(t *testing.T) {
	s := newMigrationIDTestStore(t)
	before := make([]migration, 0, len(migrations)-1)
	for _, m := range migrations {
		if strings.Contains(m.ID(), "admission-hold") {
			break
		}
		before = append(before, m)
	}
	if len(before) == 0 || len(before) == len(migrations) {
		t.Fatal("admission hold migration is missing from the catalog")
	}
	if err := s.migrateWithMigrations(t.Context(), nil, before); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE schema_migrations SET applied_at=123`); err != nil {
		t.Fatal(err)
	}
	if err := s.migrateWithMigrations(t.Context(), nil, migrations); err != nil {
		t.Fatal(err)
	}
	var preserved int
	if err := s.db.QueryRow(`SELECT count(*) FROM schema_migrations WHERE applied_at=123`).Scan(&preserved); err != nil || preserved != len(before) {
		t.Fatal(preserved, err)
	}
	hold, err := s.AdmissionHold(t.Context())
	if err != nil || hold.On || !hold.ExpiresAt.IsZero() {
		t.Fatal(hold, err)
	}
	if _, err = s.SetAdmissionHold(t.Context(), "admin", model.AdmissionHoldRequest{On: true, Reason: "post-upgrade"}); err != nil {
		t.Fatal(err)
	}
	if err = s.migrateWithMigrations(t.Context(), nil, migrations); err != nil {
		t.Fatal(err)
	}
	hold, err = s.AdmissionHold(t.Context())
	if err != nil || !hold.On || hold.Reason != "post-upgrade" {
		t.Fatal("reopen reset the hold", hold, err)
	}
}
