package store

import (
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestSubmissionPriorityExpiryAndSupersessionExceptions(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	enqueue := func(actor, ref string, p model.RunPriority, supersede bool) model.EnqueueRunResult {
		t.Helper()
		spec := testRunSpec(repo.ID, ref, strings.Repeat("a", 40))
		spec.Actor = actor
		spec.PriorityRequest = model.PriorityRequest{Priority: p, PriorityReason: "urgent", Supersede: supersede}
		r, err := s.EnqueueRun(t.Context(), spec)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if _, err := s.SetPriorityGrant(t.Context(), "admin", model.PriorityGrant{Identity: "owner", MaxClass: model.PriorityHighest, Expires: now.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	priority := enqueue("owner", "urgent", model.PriorityHighest, false)
	// A new priority build on the same branch never cancels the first one.
	second := enqueue("owner", "urgent", model.PriorityHigh, false)
	// Even an explicit supersede from somebody else cannot cancel priority work.
	_ = enqueue("other", "urgent", "", true)
	for _, r := range []model.Run{priority.Run, second.Run} {
		got, err := s.Run(t.Context(), r.ID)
		if err != nil || got.Status != model.RunQueued {
			t.Fatalf("priority cancelled: %+v %v", got, err)
		}
	}
	// Its own requester can explicitly replace it with a new normal build.
	_ = enqueue("owner", "urgent", "", true)
	for _, r := range []model.Run{priority.Run, second.Run} {
		got, err := s.Run(t.Context(), r.ID)
		if err != nil || got.Status != model.RunInterrupted {
			t.Fatalf("explicit replacement ignored: %+v %v", got, err)
		}
	}
	now = now.Add(time.Minute)
	expired := enqueue("owner", "expired", model.PriorityHighest, false)
	if expired.Priority != model.PriorityNormal || !strings.Contains(expired.PriorityRefusal, "expired") {
		t.Fatalf("expired grant accepted: %+v", expired)
	}
}

func TestRCProtectionSurvivesPatternRemoval(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	if _, err := s.SetQueuePriorityPatterns(t.Context(), "admin", []string{"rc/*"}); err != nil {
		t.Fatal(err)
	}
	first, err := s.EnqueueRun(t.Context(), testRunSpec(repo.ID, "rc/v1", strings.Repeat("a", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SetQueuePriorityPatterns(t.Context(), "admin", nil); err != nil {
		t.Fatal(err)
	}
	if _, err = s.EnqueueRun(t.Context(), testRunSpec(repo.ID, "rc/v1", strings.Repeat("b", 40))); err != nil {
		t.Fatal(err)
	}
	got, err := s.Run(t.Context(), first.ID)
	if err != nil || got.Status != model.RunQueued {
		t.Fatalf("accepted RC lost protection: %+v %v", got, err)
	}
}

func TestPrioritySubmissionKeepsRunningSameBranch(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	original, err := s.EnqueueRun(t.Context(), testRunSpec(repo.ID, "urgent", strings.Repeat("a", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimNextRun(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err = s.MarkRunAdmitted(t.Context(), original.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SetRunWorkflowName(t.Context(), original.ID, "existing-workflow"); err != nil {
		t.Fatal(err)
	}
	admin := true
	spec := testRunSpec(repo.ID, "urgent", strings.Repeat("b", 40))
	spec.PriorityRequest = model.PriorityRequest{Priority: model.PriorityHighest, UnscopedAdmin: &admin}
	urgent, err := s.EnqueueRun(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Run(t.Context(), original.ID)
	if err != nil || got.Status != model.RunRunning || got.StartedAt == nil || got.WorkflowName != "existing-workflow" || got.SupersededBy != "" || len(urgent.Cancellations) != 0 {
		t.Fatalf("priority submission changed running work: %+v %v", got, err)
	}
	if pending, err := s.PendingRunCancellations(t.Context()); err != nil || len(pending) != 0 {
		t.Fatalf("priority created cleanup obligations: %+v %v", pending, err)
	}
}

func TestPriorityRestartKeepsNewerNormalSiblingAndRequest(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	admin := true
	spec := testRunSpec(repo.ID, "urgent", strings.Repeat("a", 40))
	spec.Actor = "requester"
	spec.PriorityRequest = model.PriorityRequest{Priority: model.PriorityHighest, PriorityReason: "urgent recovery", UnscopedAdmin: &admin}
	original, err := s.EnqueueRun(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimNextRun(t.Context()); err != nil {
		t.Fatal(err)
	}
	sibling, err := s.EnqueueRun(t.Context(), testRunSpec(repo.ID, "urgent", strings.Repeat("b", 40)))
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := s.RequeueStrandedRun(t.Context(), original.ID)
	if err != nil || recovered.Priority != model.PriorityHighest || recovered.PriorityRequestedBy != "requester" || recovered.PriorityReason != "submission priority: urgent recovery" || recovered.AdmissionSequence != original.AdmissionSequence {
		t.Fatalf("priority recovery lost request/place: %+v %v", recovered, err)
	}
	newer, err := s.Run(t.Context(), sibling.ID)
	if err != nil || newer.Status != model.RunQueued || newer.SupersededBy != "" {
		t.Fatalf("recovery cancelled newer normal push: %+v %v", newer, err)
	}
	old, err := s.Run(t.Context(), original.ID)
	if err != nil || old.SupersededBy != recovered.ID {
		t.Fatalf("recovery source not linked: %+v %v", old, err)
	}
}

func TestSyncPriorityRequestPreservesAcceptedPriorityAndAuditsRefusals(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	run, err := s.EnqueueRun(t.Context(), testRunSpec(repo.ID, "sync", strings.Repeat("a", 40)))
	if err != nil {
		t.Fatal(err)
	}
	admin := true
	accepted, err := s.RequestRunPriority(t.Context(), "operator", run.ID, model.PriorityRequest{Priority: model.PriorityHigh, PriorityReason: "sync request", UnscopedAdmin: &admin}, false)
	if err != nil || accepted.Priority != model.PriorityHigh || !accepted.PriorityProtected || accepted.PriorityRequestedBy != "operator" {
		t.Fatalf("sync priority ignored: %+v %v", accepted, err)
	}
	ordinary := false
	refused, err := s.RequestRunPriority(t.Context(), "other", run.ID, model.PriorityRequest{Priority: model.PriorityHighest, UnscopedAdmin: &ordinary}, false)
	if err != nil || refused.Priority != model.PriorityHigh || refused.PriorityRefusal == "" || refused.PriorityRequestedBy != "operator" {
		t.Fatalf("refused sync request demoted priority: %+v %v", refused, err)
	}
	admitted, err := s.RequestRunPriority(t.Context(), "operator", run.ID, model.PriorityRequest{Priority: model.PriorityHighest, UnscopedAdmin: &admin}, true)
	if err != nil || admitted.Priority != model.PriorityHigh || !strings.Contains(admitted.PriorityRefusal, "admitted") {
		t.Fatalf("sync bypassed admission grant: %+v %v", admitted, err)
	}
	audits, _, err := s.ListAuditActions(t.Context(), model.AuditListFilter{ActionPrefix: "run.priority.request"})
	if err != nil || len(audits) != 3 {
		t.Fatalf("sync priority not audited: %d %v", len(audits), err)
	}
}

func TestPriorityPushUsesAuthenticatedKeyInsteadOfIdentityAlias(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	admin, _ := lifecycleUplink(t, s, "operator", "SHA256:unscoped-priority", true)
	scoped, _ := lifecycleUplink(t, s, "operator", "SHA256:scoped-priority", true)
	ordinary, _ := lifecycleUplink(t, s, "operator", "SHA256:ordinary-priority", false)
	if _, err := s.SetUplinkScope(t.Context(), "operator", scoped.Identity, scoped.Fingerprint, []string{"codeberg"}); err != nil {
		t.Fatal(err)
	}
	credential, err := s.CreateTokenCredential(t.Context(), model.TokenCredentialSpec{Name: "pending-priority", Digest: []byte(strings.Repeat("p", 32))})
	if err != nil {
		t.Fatal(err)
	}
	pending, err := s.RegisterUplink(t.Context(), "operator", model.UplinkSpec{Identity: "pending-admin", Fingerprint: "SHA256:pending-priority", TokenCredentialID: credential.ID, Admin: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.UplinkByFingerprint(t.Context(), pending.Fingerprint); err != nil {
		t.Fatalf("fixture must have an SSH-authorized pending key: %v", err)
	}
	for _, tc := range []struct {
		name, identity, fingerprint string
		want                        model.RunPriority
	}{
		{"unscoped key", admin.Identity, admin.Fingerprint, model.PriorityHighest},
		{"scoped alias", scoped.Identity, scoped.Fingerprint, model.PriorityNormal},
		{"ordinary alias", ordinary.Identity, ordinary.Fingerprint, model.PriorityNormal},
		{"missing key", admin.Identity, "", model.PriorityNormal},
		{"SSH pending admin", pending.Identity, pending.Fingerprint, model.PriorityHighest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := testRunSpec(repo.ID, strings.ReplaceAll(tc.name, " ", "-"), strings.Repeat("a", 40))
			spec.Actor = tc.identity
			spec.PriorityRequest = model.PriorityRequest{Priority: model.PriorityHighest, RequesterFingerprint: tc.fingerprint}
			run, err := s.EnqueueRun(t.Context(), spec)
			if err != nil || run.Priority != tc.want {
				t.Fatalf("key priority=%s, want %s: %v", run.Priority, tc.want, err)
			}
		})
	}
}
