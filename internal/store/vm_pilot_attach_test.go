package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/vmrunner"
)

func (f *pilotFixture) bridgeConductor(t *testing.T) vmrunner.ConductorAttempt {
	t.Helper()
	f.create(t, vmrunner.ConductorResource, "Job")
	pod := f.pod(t, "pod-conductor", vmrunner.ConductorResource)
	attempt := vmrunner.ConductorAttempt{
		AttemptID: "bridge-one", JobUID: "conductor-uid", PodUID: pod.UID,
		PodName: f.intent("pod-conductor", "Pod", vmrunner.ConductorResource).Name, NodeName: "worker-1",
		Container: "conductor", ContainerID: "containerd://" + strings.Repeat("a", 64),
		ImageDigest: "sha256:" + strings.Repeat("e", 64), SpecIdentity: pod.SpecIdentity, StartedAt: *f.now,
	}
	mustPilot(t, f.s.BindConductorAttempt(context.Background(), f.plan.Spec.RunID, attempt))
	return attempt
}

func TestMigration15PreservesSealedPilotAndStartsUnclaimed(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "pilot-v14.db")
	now := time.Now().UTC().Add(-time.Minute)
	s, err := Open(ctx, path, Options{Now: func() time.Time { return now }})
	mustPilot(t, err)
	repo := createRepo(t, s)
	plan := pilotPlan(vmIntent(t, s, repo, "migration15"))
	_, err = s.ReservePilot(ctx, plan)
	mustPilot(t, err)
	mustPilot(t, s.Close())
	db, err := sql.Open("sqlite", path)
	mustPilot(t, err)
	// Reconstruct a v14 database: undo v15 and every later migration.
	_, err = db.Exec(`ALTER TABLE vm_suite_executions DROP COLUMN attach_json;
ALTER TABLE runs DROP COLUMN concurrency_group;
DROP TABLE IF EXISTS grant_declarations;
DELETE FROM schema_migrations WHERE version>=15`)
	mustPilot(t, err)
	mustPilot(t, db.Close())
	s, err = Open(ctx, path, Options{Now: func() time.Time { return now }})
	mustPilot(t, err)
	defer func() { mustPilot(t, s.Close()) }()
	value, err := s.PilotExecution(ctx, plan.Spec.RunID)
	mustPilot(t, err)
	if value.Attach != nil || vmrunner.PilotIdentity(value.Plan) != vmrunner.PilotIdentity(plan) {
		t.Fatal("migration replaced sealed pilot or invented attach claim")
	}
}

func TestConductorAttachClaimOneUseAndPreciseBinding(t *testing.T) {
	f := newPilotFixture(t)
	*f.now = f.now.Truncate(time.Second)
	f.start(t)
	attempt := f.bridgeConductor(t)
	ctx := context.Background()
	claim, granted, err := f.s.ClaimConductorAttach(ctx, f.plan.Spec.RunID, attempt)
	mustPilot(t, err)
	if !granted || claim.AttemptIdentity != vmrunner.ConductorAttemptIdentity(attempt) || len(claim.Challenge) != 64 {
		t.Fatal("initial exact attach claim missing")
	}
	if _, granted, err = f.s.ClaimConductorAttach(ctx, f.plan.Spec.RunID, attempt); err != nil || granted {
		t.Fatalf("spent attach claim was regranted: granted=%v err=%v", granted, err)
	}
	*f.now = f.now.Add(time.Second)
	started := attempt.StartedAt.Add(300 * time.Millisecond)
	mustPilot(t, f.s.BindConductorAttach(ctx, f.plan.Spec.RunID, claim.Challenge, started))
	value, err := f.s.PilotExecution(ctx, f.plan.Spec.RunID)
	mustPilot(t, err)
	if value.Attach == nil || !value.Attach.RuntimeStartedAt.Equal(started) || value.Attach.BoundAt.IsZero() ||
		!value.Attempt.StartedAt.Equal(attempt.StartedAt) {
		t.Fatal("runtime precision or immutable Kubernetes observation was lost")
	}
	if _, granted, err = f.s.ClaimConductorAttach(ctx, f.plan.Spec.RunID, attempt); err != nil || granted {
		t.Fatalf("bound attach claim was regranted: granted=%v err=%v", granted, err)
	}
}

func TestConductorAttachClaimRejectsWrongIdentityAndBadBinding(t *testing.T) {
	for _, mode := range []string{"short-id", "wrong-pod", "wrong-node", "wrong-challenge", "wrong-start"} {
		t.Run(mode, func(t *testing.T) {
			f := newPilotFixture(t)
			*f.now = f.now.Truncate(time.Second)
			f.start(t)
			attempt := f.bridgeConductor(t)
			ctx := context.Background()
			switch mode {
			case "short-id":
				attempt.ContainerID = "containerd://a"
			case "wrong-pod":
				attempt.PodUID = "other-pod"
			case "wrong-node":
				attempt.NodeName = "other-node"
			}
			if mode == "short-id" || mode == "wrong-pod" || mode == "wrong-node" {
				if _, granted, err := f.s.ClaimConductorAttach(ctx, f.plan.Spec.RunID, attempt); !errors.Is(err, ErrInvalidState) || granted {
					t.Fatalf("wrong process was claimable: granted=%v err=%v", granted, err)
				}
				return
			}
			claim, granted, err := f.s.ClaimConductorAttach(ctx, f.plan.Spec.RunID, attempt)
			mustPilot(t, err)
			if !granted {
				t.Fatal("initial claim not granted")
			}
			*f.now = f.now.Add(time.Second)
			challenge := claim.Challenge
			started := attempt.StartedAt.Add(300 * time.Millisecond)
			if mode == "wrong-challenge" {
				challenge = strings.Repeat("0", 64)
			} else {
				started = attempt.StartedAt.Add(time.Second)
			}
			if err := f.s.BindConductorAttach(ctx, f.plan.Spec.RunID, challenge, started); !errors.Is(err, ErrInvalidState) {
				t.Fatalf("wrong binding was accepted: %v", err)
			}
			if _, granted, err := f.s.ClaimConductorAttach(ctx, f.plan.Spec.RunID, attempt); err != nil || granted {
				t.Fatalf("failed binding regranted attach: granted=%v err=%v", granted, err)
			}
		})
	}
}
