package store

import (
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestNewBranchHeadSupersedesQueuedAdminPriority(t *testing.T) {
	for _, priority := range []model.RunPriority{model.PriorityLow, model.PriorityStandard, model.PriorityPromotion, model.PriorityHigh} {
		for _, trigger := range []string{"branch", "push"} {
			t.Run(string(priority)+"/"+trigger, func(t *testing.T) {
				now := time.Now()
				s := testStore(t, &now)
				repo := createRepo(t, s)
				spec := testRunSpec(repo.ID, "feature/next", strings.Repeat("a", 40))
				spec.Trigger = trigger
				first, err := s.EnqueueRun(t.Context(), spec)
				requireStoreNoError(t, err)
				_, err = s.BumpRunPriority(t.Context(), "admin", first.ID, priority, "owner intent")
				requireStoreNoError(t, err)
				now = now.Add(time.Minute)
				spec.Actor, spec.SHA = "another-pusher", strings.Repeat("b", 40)
				// A submission request cannot erase the admin's explicit class.
				spec.Priority = model.PriorityStandard
				event := model.ReceiveEventSpec{ID: "new-head", Actor: spec.Actor, RepoID: repo.ID, RefKind: spec.RefKind, Ref: spec.Ref, OldSHA: first.SHA, ObjectSHA: spec.SHA, CommitSHA: spec.SHA, Outcome: "queued"}
				next, err := s.EnqueueReceiveEvent(t.Context(), event, spec)
				requireStoreNoError(t, err)
				old, err := s.Run(t.Context(), first.ID)
				requireStoreNoError(t, err)
				if old.Status != model.RunInterrupted || old.SupersededBy != next.ID || old.FinishedAt == nil || old.StartedAt != nil {
					t.Fatalf("stale queued run survived: %+v", old)
				}
				if next.SupersededCount != 1 || len(next.Cancellations) != 0 {
					t.Fatalf("queued supersession count/cleanup = %+v", next)
				}
				if next.Priority != priority || next.PriorityOverride != priority || next.PriorityRequestedBy != "admin" || next.PriorityOverrideReason != "owner intent" {
					t.Fatalf("admin intent lost: %+v", next.Run)
				}
				if next.QueueSequence <= first.QueueSequence || !next.QueuedAt.After(first.QueuedAt) {
					t.Fatal("new head did not earn a new FIFO place")
				}
				replay, err := s.EnqueueReceiveEvent(t.Context(), event, spec)
				requireStoreNoError(t, err)
				if !replay.Duplicate || replay.ID != next.ID || replay.Priority != priority || replay.SupersededCount != 1 {
					t.Fatalf("receive replay changed priority: %+v", replay)
				}
				audits, _, err := s.ListAuditActions(t.Context(), model.AuditListFilter{ActionPrefix: "run.priority.inherit"})
				requireStoreNoError(t, err)
				if len(audits) != 1 || !strings.Contains(audits[0].Details, first.ID) {
					t.Fatalf("inheritance audit missing or replayed: %+v", audits)
				}
				spec.SHA = strings.Repeat("c", 40)
				latest, err := s.EnqueueRun(t.Context(), spec)
				requireStoreNoError(t, err)
				if latest.PriorityOverride != priority || latest.PriorityRequestedBy != "admin" {
					t.Fatalf("next push lost inherited override: %+v", latest)
				}
			})
		}
	}
}

func TestNewBranchHeadKeepsRunningPriorityProtection(t *testing.T) {
	for _, priority := range []model.RunPriority{model.PriorityLow, model.PriorityStandard, model.PriorityPromotion, model.PriorityHigh} {
		for _, admitted := range []bool{false, true} {
			t.Run(string(priority)+"/admitted="+map[bool]string{false: "no", true: "yes"}[admitted], func(t *testing.T) {
				now := time.Now()
				s := testStore(t, &now)
				repo := createRepo(t, s)
				spec := testRunSpec(repo.ID, "feature/running", strings.Repeat("a", 40))
				first, err := s.EnqueueRun(t.Context(), spec)
				requireStoreNoError(t, err)
				_, err = s.BumpRunPriority(t.Context(), "admin", first.ID, priority, "keep running")
				requireStoreNoError(t, err)
				_, err = s.ClaimNextRun(t.Context())
				requireStoreNoError(t, err)
				if admitted {
					_, err = s.MarkRunAdmitted(t.Context(), first.ID)
					requireStoreNoError(t, err)
				}
				spec.SHA = strings.Repeat("b", 40)
				next, err := s.EnqueueRun(t.Context(), spec)
				requireStoreNoError(t, err)
				old, err := s.Run(t.Context(), first.ID)
				requireStoreNoError(t, err)
				if old.Status != model.RunRunning || old.SupersededBy != "" || len(next.Cancellations) != 0 || next.PriorityOverride != "" {
					t.Fatalf("running rule changed: old=%+v next=%+v", old, next)
				}
			})
		}
	}
}

func TestNewBranchHeadPrioritySupersessionBoundaries(t *testing.T) {
	for _, boundary := range []string{"same SHA", "different ref", "different repo", "schedule lane", "tag", "promotion", "pending publication"} {
		t.Run(boundary, func(t *testing.T) {
			now := time.Now()
			s := testStore(t, &now)
			repo := createRepo(t, s)
			spec := testRunSpec(repo.ID, "main", strings.Repeat("a", 40))
			switch boundary {
			case "schedule lane":
				spec.Trigger, spec.ScheduleEntry = "schedule", "nightly"
			case "tag":
				spec.RefKind, spec.Trigger = model.RefTag, "tag"
			case "promotion":
				spec.Trigger = "promotion"
			}
			first, err := s.EnqueueRun(t.Context(), spec)
			requireStoreNoError(t, err)
			_, err = s.BumpRunPriority(t.Context(), "admin", first.ID, model.PriorityHigh, "protected lane")
			requireStoreNoError(t, err)
			if boundary == "pending publication" {
				_, err = s.db.Exec(`INSERT INTO publications(id,repo_id,run_id,ref_kind,ref,result_sha,actor,created_at,updated_at) VALUES('publication',?,?,'branch',?,?,'oberth',?,?)`, repo.ID, first.ID, first.Ref, first.SHA, now.UnixNano(), now.UnixNano())
				requireStoreNoError(t, err)
			}
			spec = testRunSpec(repo.ID, "main", strings.Repeat("b", 40))
			switch boundary {
			case "same SHA":
				spec.SHA = first.SHA
			case "different ref":
				spec.Ref = "another"
			case "different repo":
				other, err := s.CreateRepository(t.Context(), model.RepositorySpec{UpstreamID: repo.UpstreamID, Name: "other", DefaultBranch: "main"})
				requireStoreNoError(t, err)
				spec.RepoID = other.ID
			}
			next, err := s.EnqueueRun(t.Context(), spec)
			requireStoreNoError(t, err)
			old, err := s.Run(t.Context(), first.ID)
			requireStoreNoError(t, err)
			if old.Status != model.RunQueued || old.SupersededBy != "" || next.PriorityOverride != "" {
				t.Fatalf("crossed %s boundary: old=%+v next=%+v", boundary, old, next)
			}
		})
	}
}

func TestNewBranchHeadInheritsLatestAdminDecisionAtomically(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	spec := testRunSpec(repo.ID, "main", strings.Repeat("a", 40))
	first, err := s.EnqueueRun(t.Context(), spec)
	requireStoreNoError(t, err)
	_, err = s.BumpRunPriority(t.Context(), "first-admin", first.ID, model.PriorityHigh, "first decision")
	requireStoreNoError(t, err)
	second, err := s.EnqueueRun(t.Context(), spec) // Same SHA is not a new head.
	requireStoreNoError(t, err)
	_, err = s.BumpRunPriority(t.Context(), "second-admin", second.ID, model.PriorityHigh, "other decision")
	requireStoreNoError(t, err)
	now = now.Add(time.Minute)
	_, err = s.BumpRunPriority(t.Context(), "first-admin", first.ID, model.PriorityLow, "latest lowering")
	requireStoreNoError(t, err)
	spec.SHA = strings.Repeat("b", 40)
	admin := true
	spec.PriorityRequest = model.PriorityRequest{Priority: model.PriorityHigh, UnscopedAdmin: &admin}
	_, err = s.db.Exec(`CREATE TRIGGER reject_priority_inheritance BEFORE INSERT ON audit_actions WHEN NEW.action='run.priority.inherit' BEGIN SELECT RAISE(ABORT,'audit unavailable'); END`)
	requireStoreNoError(t, err)
	if _, err = s.EnqueueRun(t.Context(), spec); err == nil {
		t.Fatal("enqueue committed without inheritance audit")
	}
	for _, id := range []string{first.ID, second.ID} {
		old, err := s.Run(t.Context(), id)
		requireStoreNoError(t, err)
		if old.Status != model.RunQueued {
			t.Fatalf("failed enqueue superseded source: %+v", old)
		}
	}
	var count int
	requireStoreNoError(t, s.db.QueryRow(`SELECT count(*) FROM runs`).Scan(&count))
	if count != 2 {
		t.Fatal("failed enqueue left a replacement", count)
	}
	_, err = s.db.Exec(`DROP TRIGGER reject_priority_inheritance`)
	requireStoreNoError(t, err)
	next, err := s.EnqueueRun(t.Context(), spec)
	requireStoreNoError(t, err)
	if next.Priority != model.PriorityLow || next.PriorityRequestedBy != "first-admin" || next.PriorityOverrideReason != "latest lowering" || next.SupersededCount != 2 {
		t.Fatalf("latest admin decision did not win: %+v", next)
	}
}

func TestInheritedBranchPriorityDoesNotChangeRunningSupersession(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	spec := testRunSpec(repo.ID, "main", strings.Repeat("a", 40))
	running, err := s.EnqueueRun(t.Context(), spec)
	requireStoreNoError(t, err)
	_, err = s.ClaimNextRun(t.Context())
	requireStoreNoError(t, err)
	_, err = s.MarkRunAdmitted(t.Context(), running.ID)
	requireStoreNoError(t, err)
	_, err = s.SetRunWorkflowName(t.Context(), running.ID, "running-workflow")
	requireStoreNoError(t, err)
	admin := true
	spec.SHA = strings.Repeat("b", 40)
	spec.PriorityRequest = model.PriorityRequest{Priority: model.PriorityHigh, UnscopedAdmin: &admin}
	queued, err := s.EnqueueRun(t.Context(), spec)
	requireStoreNoError(t, err)
	_, err = s.BumpRunPriority(t.Context(), "admin", queued.ID, model.PriorityHigh, "keep next head urgent")
	requireStoreNoError(t, err)
	spec.SHA = strings.Repeat("c", 40)
	spec.PriorityRequest = model.PriorityRequest{}
	next, err := s.EnqueueRun(t.Context(), spec)
	requireStoreNoError(t, err)
	if next.Priority != model.PriorityHigh || next.SupersededCount != 2 || len(next.Cancellations) != 1 || next.Cancellations[0].RunID != running.ID || next.Cancellations[0].WorkflowName != "running-workflow" {
		t.Fatalf("inherited class changed running cancellation or count: %+v", next)
	}
	for _, id := range []string{running.ID, queued.ID} {
		old, err := s.Run(t.Context(), id)
		requireStoreNoError(t, err)
		if old.Status != model.RunInterrupted || old.SupersededBy != next.ID {
			t.Fatalf("supersession missed old run: %+v", old)
		}
	}
}
