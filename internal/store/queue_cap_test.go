package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

type queueCapContract interface {
	SetQueueCap(context.Context, string, int) (model.QueueCapStatus, error)
	QueueCap(context.Context) (model.QueueCapStatus, error)
	DrainDeferredBranch(context.Context) (model.EnqueueRunResult, error)
}

func capStore(t *testing.T, s *Store) queueCapContract {
	t.Helper()
	c, ok := any(s).(queueCapContract)
	if !ok {
		t.Fatal("queue cap persistence is unavailable")
	}
	return c
}

func capReceive(repo int64, id, trigger string) (model.ReceiveEventSpec, model.RunSpec) {
	sha := strings.Repeat("a", 40)
	e := model.ReceiveEventSpec{ID: id, Actor: "dev", RepoID: repo, RefKind: model.RefBranch, Ref: "feature/" + id, ObjectSHA: sha, CommitSHA: sha, Outcome: "queued"}
	r := model.RunSpec{RepoID: repo, Actor: e.Actor, RefKind: e.RefKind, Ref: e.Ref, SHA: sha, TestedSHA: sha, Trigger: trigger}
	return e, r
}

func TestQueueCapOffReachedDrainAndReplay(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	c := capStore(t, s)
	repo := createRepo(t, s)
	ctx := t.Context()
	status, err := c.QueueCap(ctx)
	if err != nil || status.MaxNotStarted != 0 {
		t.Fatalf("default: %+v %v", status, err)
	}
	e, spec := capReceive(repo.ID, "first", "branch")
	first, err := s.EnqueueReceiveEvent(ctx, e, spec)
	if err != nil || first.ID == "" {
		t.Fatalf("cap off: %+v %v", first, err)
	}
	if _, err := c.SetQueueCap(ctx, "admin", 1); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"second", "third"} {
		e, spec := capReceive(repo.ID, id, "branch")
		for attempt := range 2 {
			got, err := s.EnqueueReceiveEvent(ctx, e, spec)
			if err != nil || got.ID != "" || got.Duplicate != (attempt == 1) {
				t.Fatalf("defer %s attempt %d: %+v %v", id, attempt, got, err)
			}
		}
	}
	status, err = c.QueueCap(ctx)
	if err != nil || status.NotStarted != 1 || status.Deferred != 2 || status.Reason != "branch run deferred: queue at 1/1" {
		t.Fatalf("full: %+v %v", status, err)
	}
	if got, err := c.DrainDeferredBranch(ctx); err != nil || got.ID != "" {
		t.Fatalf("drained full queue: %+v %v", got, err)
	}
	claimed, err := s.ClaimNextRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	status, err = c.QueueCap(ctx)
	if err != nil || status.NotStarted != 1 {
		t.Fatalf("waiting claim lost from count: %+v %v", status, err)
	}
	if _, err := s.FinishRun(ctx, claimed.ID, model.RunResult{Status: model.RunPassed}); err != nil {
		t.Fatal(err)
	}
	previous := first.QueueSequence
	for _, id := range []string{"second", "third"} {
		got, err := c.DrainDeferredBranch(ctx)
		if err != nil || got.Ref != "feature/"+id || got.QueueSequence <= previous || !strings.Contains(got.Reason, "branch run deferred: queue at 1/1") {
			t.Fatalf("drain %s: %+v %v", id, got, err)
		}
		previous = got.QueueSequence
		e, spec := capReceive(repo.ID, id, "branch")
		replay, err := s.EnqueueReceiveEvent(ctx, e, spec)
		if err != nil || replay.ID != got.ID || !replay.Duplicate {
			t.Fatalf("drained replay: %+v %v", replay, err)
		}
		if _, err := s.ClaimNextRun(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := s.FinishRun(ctx, got.ID, model.RunResult{Status: model.RunPassed}); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_actions WHERE resource_type='receive' AND details LIKE '%branch run deferred: queue at 1/1%'`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("deferral audit count %d: %v", count, err)
	}
}

func TestQueueCapBypassDisableAndValidation(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	c := capStore(t, s)
	repo := createRepo(t, s)
	ctx := t.Context()
	for _, max := range []int{-1, -100} {
		if _, err := c.SetQueueCap(ctx, "admin", max); !errors.Is(err, ErrInvalid) {
			t.Fatalf("negative cap: %v", err)
		}
	}
	if _, err := c.SetQueueCap(ctx, "", 1); !errors.Is(err, ErrInvalid) {
		t.Fatalf("missing actor: %v", err)
	}
	if _, err := c.SetQueueCap(ctx, "admin", 1); err != nil {
		t.Fatal(err)
	}
	for i, trigger := range []string{"branch", "promotion", "schedule", "tag", "branch"} {
		e, spec := capReceive(repo.ID, fmt.Sprintf("bypass-%d", i), trigger)
		if trigger == "tag" {
			e.RefKind = model.RefTag
			spec.RefKind = model.RefTag
			spec.Release = true
			spec.Credentialed = true
		}
		got, err := s.EnqueueReceiveEvent(ctx, e, spec)
		if err != nil || (got.ID == "") != (i == 4) {
			t.Fatalf("%s: %+v %v", trigger, got, err)
		}
	}
	if _, err := c.SetQueueCap(ctx, "admin", 0); err != nil {
		t.Fatal(err)
	}
	got, err := c.DrainDeferredBranch(ctx)
	if err != nil || got.ID == "" {
		t.Fatalf("disable did not release backlog: %+v %v", got, err)
	}
}

func TestQueueCapRestartPersistenceAndArrivalOrder(t *testing.T) {
	now := time.Now()
	path := filepath.Join(t.TempDir(), "queue.sqlite")
	s := openTestStore(t, path, &now)
	c := capStore(t, s)
	repo := createRepo(t, s)
	ctx := t.Context()
	if _, err := c.SetQueueCap(ctx, "admin", 1); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"occupant", "z-first", "a-second"} {
		e, r := capReceive(repo.ID, id, "branch")
		if _, err := s.EnqueueReceiveEvent(ctx, e, r); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openTestStore(t, path, &now)
	c = capStore(t, s)
	status, err := c.QueueCap(ctx)
	if err != nil || status.MaxNotStarted != 1 || status.Deferred != 2 {
		t.Fatalf("restarted: %+v %v", status, err)
	}
	if _, err := c.SetQueueCap(ctx, "admin", 0); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"z-first", "a-second"} {
		got, err := c.DrainDeferredBranch(ctx)
		if err != nil || got.Ref != "feature/"+want {
			t.Fatalf("restart FIFO %s: %+v %v", want, got, err)
		}
	}
}

func TestQueueCapConcurrentArrivalsAndNoLeapfrog(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	c := capStore(t, s)
	repo := createRepo(t, s)
	ctx := t.Context()
	if _, err := c.SetQueueCap(ctx, "admin", 1); err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 12)
	other, err := OpenAdminClient(ctx, s.livePath, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close() }()
	for i := range 12 {
		go func() {
			e, r := capReceive(repo.ID, fmt.Sprintf("arrival-%d", i), "branch")
			client := s
			if i%2 != 0 {
				client = other
			}
			_, err := client.EnqueueReceiveEvent(ctx, e, r)
			results <- err
		}()
	}
	for range 12 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	cap, err := c.QueueCap(ctx)
	if err != nil || cap.NotStarted != 1 || cap.Deferred != 11 {
		t.Fatalf("concurrent cap: %+v %v", cap, err)
	}
	claimed, err := s.ClaimNextRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetRunWorkflowName(ctx, claimed.ID, "test-started"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkRunSubmitted(ctx, claimed.ID); err != nil {
		t.Fatal(err)
	}
	e, r := capReceive(repo.ID, "latest", "branch")
	late, err := s.EnqueueReceiveEvent(ctx, e, r)
	if err != nil || late.ID != "" {
		t.Fatalf("new arrival leapfrogged backlog: %+v %v", late, err)
	}
	oldest, err := c.DrainDeferredBranch(ctx)
	if err != nil || oldest.ID == "" || oldest.Ref == "feature/latest" {
		t.Fatalf("drain did not use opened slot: %+v %v", oldest, err)
	}
	cap, err = c.QueueCap(ctx)
	if err != nil || cap.NotStarted != 1 || cap.Deferred != 11 {
		t.Fatalf("started workflow still counted: %+v %v", cap, err)
	}
}

func TestQueueCapAuditFailuresRollbackAdmissionAndDrain(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := t.Context()
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER reject_queue_cap_config BEFORE INSERT ON audit_actions WHEN NEW.action='queue.cap.config' BEGIN SELECT RAISE(ABORT,'audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetQueueCap(ctx, "admin", 1); err == nil {
		t.Fatal("cap configuration ignored audit failure")
	}
	cap, err := s.QueueCap(ctx)
	if err != nil || cap.MaxNotStarted != 0 {
		t.Fatalf("failed setting persisted: %+v %v", cap, err)
	}
	if _, err := s.db.ExecContext(ctx, `DROP TRIGGER reject_queue_cap_config`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetQueueCap(ctx, "admin", 1); err != nil {
		t.Fatal(err)
	}
	e, r := capReceive(repo.ID, "occupant", "branch")
	first, err := s.EnqueueReceiveEvent(ctx, e, r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER reject_queue_cap_receive BEFORE INSERT ON audit_actions WHEN NEW.resource_type='receive' BEGIN SELECT RAISE(ABORT,'audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	e, r = capReceive(repo.ID, "deferred", "branch")
	if _, err := s.EnqueueReceiveEvent(ctx, e, r); err == nil {
		t.Fatal("deferral ignored audit failure")
	}
	cap, err = s.QueueCap(ctx)
	if err != nil || cap.Deferred != 0 {
		t.Fatalf("unaudited deferral persisted: %+v %v", cap, err)
	}
	if _, err := s.db.ExecContext(ctx, `DROP TRIGGER reject_queue_cap_receive`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnqueueReceiveEvent(ctx, e, r); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinishRun(ctx, first.ID, model.RunResult{Status: model.RunPassed}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER reject_queue_cap_drain BEFORE INSERT ON audit_actions WHEN NEW.action='queue.cap.drain' BEGIN SELECT RAISE(ABORT,'audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DrainDeferredBranch(ctx); err == nil {
		t.Fatal("drain ignored audit failure")
	}
	cap, err = s.QueueCap(ctx)
	if err != nil || cap.Deferred != 1 || cap.NotStarted != 0 {
		t.Fatalf("partial drain persisted: %+v %v", cap, err)
	}
	replay, err := s.EnqueueReceiveEvent(ctx, e, r)
	if err != nil || !replay.Duplicate || replay.ID != "" {
		t.Fatalf("failed drain lost receive identity: %+v %v", replay, err)
	}
	if _, err := s.db.ExecContext(ctx, `DROP TRIGGER reject_queue_cap_drain`); err != nil {
		t.Fatal(err)
	}
	if got, err := s.DrainDeferredBranch(ctx); err != nil || got.ID == "" {
		t.Fatalf("retry drain: %+v %v", got, err)
	}
}

func TestQueueCapRechecksPriorityAuthorityAndRetainsSupersessionAfterRestart(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := t.Context()
	admin, _ := lifecycleUplink(t, s, "operator", "SHA256:queue-cap-admin", true)
	if _, err := s.SetQueueCap(ctx, "operator", 1); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"occupant", "priority", "revoked", "supersede"} {
		e, r := capReceive(repo.ID, id, "branch")
		if id != "occupant" {
			e.Actor = admin.Identity
			r.Actor = admin.Identity
			r.PriorityRequest = model.PriorityRequest{Priority: model.PriorityHighest, PriorityReason: "urgent", RequesterFingerprint: admin.Fingerprint}
		}
		if id == "supersede" {
			e.Ref = "feature/priority"
			r.Ref = e.Ref
			r.Priority = ""
			r.Supersede = true
		}
		if _, err := s.EnqueueReceiveEvent(ctx, e, r); err != nil {
			t.Fatal(err)
		}
	}
	path := s.livePath
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openTestStore(t, path, &now)
	if _, err := s.SetQueueCap(ctx, "operator", 0); err != nil {
		t.Fatal(err)
	}
	first, err := s.DrainDeferredBranch(ctx)
	if err != nil || first.Priority != model.PriorityHighest || first.PriorityReason != "submission priority: urgent" || first.PriorityAcceptedReason != "urgent" {
		t.Fatalf("lost admin key or priority after restart: %+v %v", first, err)
	}
	if _, err := s.SetUplinkScope(ctx, "operator", admin.Identity, admin.Fingerprint, []string{"codeberg"}); err != nil {
		t.Fatal(err)
	}
	second, err := s.DrainDeferredBranch(ctx)
	if err != nil || second.Priority != model.PriorityNormal || second.PriorityRefusal == "" {
		t.Fatalf("reused stale admin authority: %+v %v", second, err)
	}
	third, err := s.DrainDeferredBranch(ctx)
	if err != nil || third.Ref != "feature/priority" {
		t.Fatalf("supersede drain: %+v %v", third, err)
	}
	old, err := s.Run(ctx, first.ID)
	if err != nil || old.Status != model.RunInterrupted || old.SupersededBy != third.ID {
		t.Fatalf("lost explicit supersession after restart: %+v %v", old, err)
	}
}
