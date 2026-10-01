package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/vmrunner"
)

type pilotFixture struct {
	s    *Store
	now  *time.Time
	plan vmrunner.PilotPlan
	repo model.Repository
}

func newPilotFixture(t *testing.T) *pilotFixture {
	t.Helper()
	now := time.Now().UTC().Add(-time.Minute)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	plan := pilotPlan(vmIntent(t, s, repo, "pilot"))
	return &pilotFixture{s: s, now: &now, plan: plan, repo: repo}
}

func pilotPlan(intent vmrunner.ExecutionIntent) vmrunner.PilotPlan {
	intent.Spec.Deadline = 5 * time.Minute
	return vmrunner.PilotPlan{Version: 1, Profile: vmrunner.BeaconTransportProfile, Spec: intent.Spec,
		ConductorImageRef: "registry.example/conductor@sha256:" + strings.Repeat("e", 64),
		KernelDigest:      "sha256:" + strings.Repeat("1", 64), InitramfsDigest: "sha256:" + strings.Repeat("2", 64),
		GuestHelperDigest: "sha256:" + strings.Repeat("3", 64), ArtifactDigest: "sha256:" + strings.Repeat("4", 64), ArtifactBytes: 1024,
		GuestNamespace: "pilot-guest", ConductorNamespace: "pilot-conductor", ServerNamespace: "oberth"}
}

func mustPilot(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (f *pilotFixture) reserve(t *testing.T) {
	t.Helper()
	_, err := f.s.ReservePilot(context.Background(), f.plan)
	mustPilot(t, err)
}

func (f *pilotFixture) start(t *testing.T) {
	t.Helper()
	f.reserve(t)
	mustPilot(t, f.s.SubmitPilot(context.Background(), f.plan.Spec.RunID))
}

func (f *pilotFixture) intent(key, kind, parent string) vmrunner.ResourceIntent {
	namespace := f.plan.GuestNamespace
	if key == vmrunner.ConductorResource || key == "conductor-isolation" || parent == vmrunner.ConductorResource {
		namespace = f.plan.ConductorNamespace
	}
	return vmrunner.ResourceIntent{Key: key, Kind: kind, Namespace: namespace, Name: "owned-" + key, Parent: parent, SpecIdentity: fmt.Sprintf("%x", sha256.Sum256([]byte(key)))}
}

func (f *pilotFixture) create(t *testing.T, key, kind string) vmrunner.ResourceReceipt {
	t.Helper()
	ctx := context.Background()
	intent := f.intent(key, kind, "")
	mustPilot(t, f.s.AddPilotResource(ctx, f.plan.Spec.RunID, intent))
	mustPilot(t, f.s.SubmitPilotResource(ctx, f.plan.Spec.RunID, key))
	receipt := vmrunner.ResourceReceipt{UID: key + "-uid", SpecIdentity: intent.SpecIdentity, CreatedAt: *f.now}
	mustPilot(t, f.s.BindPilotResource(ctx, f.plan.Spec.RunID, key, receipt))
	return receipt
}

func (f *pilotFixture) pod(t *testing.T, key, parent string) vmrunner.ResourceReceipt {
	t.Helper()
	intent := f.intent(key, "Pod", parent)
	receipt := vmrunner.ResourceReceipt{UID: key + "-uid", SpecIdentity: intent.SpecIdentity, OwnerUID: parent + "-uid", CreatedAt: *f.now, PodIP: "10.42.1.10"}
	mustPilot(t, f.s.ObservePilotPod(context.Background(), f.plan.Spec.RunID, intent, receipt))
	return receipt
}

func (f *pilotFixture) clean(t *testing.T, key string) {
	t.Helper()
	ctx := context.Background()
	resources, err := f.s.PilotResources(ctx, f.plan.Spec.RunID)
	mustPilot(t, err)
	for _, resource := range resources {
		if resource.Intent.Key != key {
			continue
		}
		mustPilot(t, f.s.BeginPilotResourceCleanup(ctx, f.plan.Spec.RunID, key))
		mustPilot(t, f.s.CompletePilotResourceCleanup(ctx, f.plan.Spec.RunID, key, resource.Receipt))
		return
	}
	t.Fatalf("missing fixture resource %s", key)
}

func (f *pilotFixture) conductor(t *testing.T) vmrunner.ConductorAttempt {
	t.Helper()
	attempt := f.bridgeConductor(t)
	claim, granted, err := f.s.ClaimConductorAttach(context.Background(), f.plan.Spec.RunID, attempt)
	mustPilot(t, err)
	if !granted {
		t.Fatal("fixture attach was not claimed")
	}
	mustPilot(t, f.s.BindConductorAttach(context.Background(), f.plan.Spec.RunID, claim.Challenge, attempt.StartedAt))
	return attempt
}

func (f *pilotFixture) receipt(attempt vmrunner.ConductorAttempt) vmrunner.PilotReceipt {
	return vmrunner.PilotReceipt{Plan: f.plan, Attempt: attempt,
		Termination: vmrunner.ConductorTermination{Attempt: attempt, Reason: "Completed", FinishedAt: *f.now},
		Results:     vmrunner.ConductorResults{Execution: vmrunner.PilotIdentity(f.plan), Attempt: vmrunner.ConductorAttemptIdentity(attempt), Inventory: vmrunner.PilotInventoryIdentity(f.plan), Cases: vmrunner.BeaconCases(), Complete: true}}
}

func (f *pilotFixture) endpoint(attempt string) vmrunner.PilotEndpoint {
	leaf := "5"
	if attempt == vmrunner.RestartVMResource {
		leaf = "6"
	}
	return vmrunner.PilotEndpoint{Attempt: attempt, VMIUID: attempt + "-uid", LauncherUID: "pod-" + attempt + "-uid", Address: "10.42.1.10:443", ServerName: "beacon.fixture", CertificateSHA256: strings.Repeat(leaf, 64), FixtureGeneration: strings.Repeat("7", 64)}
}

func (f *pilotFixture) claimRestart(t *testing.T, attempt vmrunner.ConductorAttempt) vmrunner.PilotRestartRequest {
	t.Helper()
	ctx := context.Background()
	endpoint := f.endpoint(vmrunner.InitialVMResource)
	mustPilot(t, f.s.BindPilotEndpoint(ctx, f.plan.Spec.RunID, endpoint))
	request := vmrunner.PilotRestartRequest{Version: 1, Type: "restart-beacon", Execution: vmrunner.PilotIdentity(f.plan), Attempt: vmrunner.ConductorAttemptIdentity(attempt), Inventory: vmrunner.PilotInventoryIdentity(f.plan), Sequence: 1, Case: "restart-fresh-relay", Old: endpoint}
	claimed, err := f.s.ClaimPilotRestart(ctx, f.plan.Spec.RunID, request)
	mustPilot(t, err)
	if !claimed {
		t.Fatal("first restart was not claimed")
	}
	return request
}

func (f *pilotFixture) restartResponse(request vmrunner.PilotRestartRequest) vmrunner.PilotRestartResponse {
	return vmrunner.PilotRestartResponse{Version: 1, Type: "restart-complete", Execution: request.Execution, Attempt: request.Attempt, Operation: vmrunner.PilotRestartIdentity(request), OldVMIUID: request.Old.VMIUID, Endpoint: f.endpoint(vmrunner.RestartVMResource)}
}

func TestPilotJournalPublicationRequiresAllResourceCleanupAndPolicyIdentity(t *testing.T) {
	ctx := context.Background()
	f := newPilotFixture(t)
	f.start(t)
	f.create(t, "guest-isolation", "NetworkPolicy")
	f.create(t, "conductor-isolation", "NetworkPolicy")
	f.create(t, "beacon-endpoint", "Service")
	f.create(t, vmrunner.InitialVMResource, "VirtualMachineInstance")
	f.pod(t, "pod-beacon-0", vmrunner.InitialVMResource)
	attempt := f.conductor(t)
	request := f.claimRestart(t, attempt)
	// The old VM and its launcher both prevent replacement submission.
	restart := f.intent(vmrunner.RestartVMResource, "VirtualMachineInstance", "")
	mustPilot(t, f.s.AddPilotResource(ctx, f.plan.Spec.RunID, restart))
	if err := f.s.SubmitPilotResource(ctx, f.plan.Spec.RunID, restart.Key); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("overlapping VM attempts: %v", err)
	}
	mustPilot(t, f.s.BeginPilotResourceCleanup(ctx, f.plan.Spec.RunID, vmrunner.InitialVMResource))
	if err := f.s.CompletePilotResourceCleanup(ctx, f.plan.Spec.RunID, vmrunner.InitialVMResource, vmrunner.ResourceReceipt{UID: "beacon-0-uid", SpecIdentity: f.intent(vmrunner.InitialVMResource, "VirtualMachineInstance", "").SpecIdentity, CreatedAt: *f.now}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("surviving launcher ignored: %v", err)
	}
	f.clean(t, "pod-beacon-0")
	f.clean(t, vmrunner.InitialVMResource)
	f.create(t, vmrunner.RestartVMResource, "VirtualMachineInstance")
	f.pod(t, "pod-beacon-1", vmrunner.RestartVMResource)
	mustPilot(t, f.s.CompletePilotRestart(ctx, f.plan.Spec.RunID, f.restartResponse(request)))
	mustPilot(t, f.s.RecordPilotReceipt(ctx, f.plan.Spec.RunID, f.receipt(attempt)))
	if _, err := vmrunner.VerifyPilotPublication(ctx, f.s, f.plan); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("provisional receipt published: %v", err)
	}
	mustPilot(t, f.s.BeginPilotCleanup(ctx, f.plan.Spec.RunID))
	for _, key := range []string{"pod-conductor", vmrunner.ConductorResource, "pod-beacon-1", vmrunner.RestartVMResource, "beacon-endpoint", "guest-isolation"} {
		f.clean(t, key)
	}
	if err := f.s.CompletePilotCleanup(ctx, f.plan.Spec.RunID); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("network cleanup omitted: %v", err)
	}
	if err := f.s.SetRunCredentialed(ctx, f.plan.Spec.RunID); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("credential promoted with network obligation: %v", err)
	}
	f.clean(t, "conductor-isolation")
	mustPilot(t, f.s.CompletePilotCleanup(ctx, f.plan.Spec.RunID))
	if _, err := vmrunner.VerifyPilotPublication(ctx, f.s, f.plan); err != nil {
		t.Fatalf("complete exact receipt unavailable: %v", err)
	}
	changed := f.plan
	changed.ArtifactDigest = "sha256:" + strings.Repeat("a", 64)
	if _, err := vmrunner.VerifyPilotPublication(ctx, f.s, changed); err == nil {
		t.Fatal("receipt reused for changed artifact")
	}
	_, err := f.s.VerifyAuditChain(ctx)
	mustPilot(t, err)
	// A new legacy VM can use the same single slot only now.
	legacy := vmIntent(t, f.s, f.repo, "next")
	_, err = f.s.ReserveVMExecution(ctx, legacy)
	mustPilot(t, err)
}

func TestPilotJournalCredentialTransitionBothOrders(t *testing.T) {
	for _, submitted := range []bool{false, true} {
		t.Run(fmt.Sprint(submitted), func(t *testing.T) {
			ctx := context.Background()
			f := newPilotFixture(t)
			f.reserve(t)
			if submitted {
				mustPilot(t, f.s.SubmitPilot(ctx, f.plan.Spec.RunID))
				if err := f.s.SetRunCredentialed(ctx, f.plan.Spec.RunID); !errors.Is(err, ErrInvalidState) {
					t.Fatalf("credential delivery allowed after pilot submit: %v", err)
				}
			} else {
				mustPilot(t, f.s.SetRunCredentialed(ctx, f.plan.Spec.RunID))
				if err := f.s.SubmitPilot(ctx, f.plan.Spec.RunID); !errors.Is(err, ErrInvalidState) {
					t.Fatalf("credentialed pilot submitted: %v", err)
				}
			}
			mustPilot(t, f.s.BeginPilotCleanup(ctx, f.plan.Spec.RunID))
			mustPilot(t, f.s.CompletePilotCleanup(ctx, f.plan.Spec.RunID))
			mustPilot(t, f.s.SetRunCredentialed(ctx, f.plan.Spec.RunID))
		})
	}
}

func TestPilotJournalUnknownCreateSurvivesRestartAndRetainsSharedCapacity(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "pilot.db")
	now := time.Now().UTC().Add(-time.Minute)
	s, err := Open(ctx, path, Options{Now: func() time.Time { return now }})
	mustPilot(t, err)
	t.Cleanup(func() { mustPilot(t, s.Close()) })
	repo := createRepo(t, s)
	f := &pilotFixture{s: s, now: &now, repo: repo, plan: pilotPlan(vmIntent(t, s, repo, "pilot"))}
	f.start(t)
	intent := f.intent(vmrunner.InitialVMResource, "VirtualMachineInstance", "")
	mustPilot(t, s.AddPilotResource(ctx, f.plan.Spec.RunID, intent))
	mustPilot(t, s.SubmitPilotResource(ctx, f.plan.Spec.RunID, intent.Key))
	mustPilot(t, s.BeginPilotCleanup(ctx, f.plan.Spec.RunID))
	mustPilot(t, s.BeginPilotResourceCleanup(ctx, f.plan.Spec.RunID, intent.Key))
	if err := s.CompletePilotResourceCleanup(ctx, f.plan.Spec.RunID, intent.Key, vmrunner.ResourceReceipt{}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("unknown create released on absence: %v", err)
	}
	mustPilot(t, s.Close())
	s, err = Open(ctx, path, Options{Now: func() time.Time { return now }})
	mustPilot(t, err)
	f.s = s
	pending, err := s.PendingPilots(ctx)
	mustPilot(t, err)
	if len(pending) != 1 || !pending[0].Cleaning {
		t.Fatalf("lost cleanup after restart: %#v", pending)
	}
	// Owner startup queues a distinct retry for the interrupted run. Claim
	// that existing queue entry before asking vmIntent to create another run;
	// it must not inherit or release the old execution's cleanup reservation.
	retry, err := s.ClaimNextRun(ctx)
	mustPilot(t, err)
	if retry.ID == f.plan.Spec.RunID || retry.Ref != "pilot" || retry.TestedSHA != f.plan.Spec.CandidateSHA {
		t.Fatalf("unexpected startup retry: %#v", retry)
	}
	legacy := vmIntent(t, s, repo, "legacy")
	if _, err := s.ReserveVMExecution(ctx, legacy); !errors.Is(err, ErrVMCapacity) {
		t.Fatalf("legacy reused unknown pilot capacity: %v", err)
	}
	second := pilotPlan(vmIntent(t, s, repo, "second"))
	if _, err := s.ReservePilot(ctx, second); !errors.Is(err, ErrVMCapacity) {
		t.Fatalf("pilot reused unknown capacity: %v", err)
	}
	receipt := vmrunner.ResourceReceipt{UID: "beacon-0-uid", SpecIdentity: intent.SpecIdentity, CreatedAt: now}
	mustPilot(t, s.BindPilotResource(ctx, f.plan.Spec.RunID, intent.Key, receipt))
	// A child first observed during recovery still blocks parent cleanup.
	f.pod(t, "late-launcher", vmrunner.InitialVMResource)
	if err := s.CompletePilotResourceCleanup(ctx, f.plan.Spec.RunID, intent.Key, receipt); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("late launcher dropped: %v", err)
	}
	f.clean(t, "late-launcher")
	f.clean(t, vmrunner.InitialVMResource)
	mustPilot(t, s.CompletePilotCleanup(ctx, f.plan.Spec.RunID))
	_, err = s.ReservePilot(ctx, second)
	mustPilot(t, err)
}

func TestPilotJournalSecondAttemptPoisonsReceiptAndRetainsChild(t *testing.T) {
	for _, kind := range []string{"container", "image", "pod"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			f := newPilotFixture(t)
			f.start(t)
			attempt := f.conductor(t)
			mustPilot(t, f.s.RecordPilotReceipt(ctx, f.plan.Spec.RunID, f.receipt(attempt)))
			var err error
			if kind == "container" || kind == "image" {
				attempt.ContainerID = "containerd://replacement"
				if kind == "image" {
					attempt.ImageDigest = "sha256:" + strings.Repeat("0", 64)
				}
				err = f.s.BindConductorAttempt(ctx, f.plan.Spec.RunID, attempt)
			} else {
				intent := f.intent("pod-replacement", "Pod", vmrunner.ConductorResource)
				err = f.s.ObservePilotPod(ctx, f.plan.Spec.RunID, intent, vmrunner.ResourceReceipt{UID: "pod-replacement-uid", OwnerUID: "conductor-uid", SpecIdentity: intent.SpecIdentity, CreatedAt: *f.now})
			}
			if !errors.Is(err, ErrInvalidState) {
				t.Fatalf("replacement accepted: %v", err)
			}
			value, err := f.s.PilotExecution(ctx, f.plan.Spec.RunID)
			mustPilot(t, err)
			if value.Failure != "attempt-replaced" || !value.Cleaning || value.Attempt.ContainerID != "containerd://"+strings.Repeat("a", 64) {
				t.Fatalf("attempt CAS/failure lost: %#v", value)
			}
			resources, err := f.s.PilotResources(ctx, f.plan.Spec.RunID)
			mustPilot(t, err)
			want := 2
			if kind == "pod" {
				want = 3
			}
			if len(resources) != want {
				t.Fatalf("lost child obligations: %d, want %d", len(resources), want)
			}
			if _, err := f.s.CompletedPilotReceipt(ctx, f.plan.Spec.RunID); !errors.Is(err, ErrInvalidState) {
				t.Fatalf("poisoned receipt usable: %v", err)
			}
		})
	}
}

func TestPilotJournalInvalidExitCommitsFailureAndAuditFailureRollsBack(t *testing.T) {
	ctx := context.Background()
	f := newPilotFixture(t)
	f.start(t)
	attempt := f.conductor(t)
	receipt := f.receipt(attempt)
	receipt.Termination.ExitCode = 7
	_, err := f.s.db.Exec(`CREATE TRIGGER fail_pilot_audit BEFORE INSERT ON audit_actions WHEN NEW.action='vm.pilot-receipt' BEGIN SELECT RAISE(ABORT,'audit unavailable'); END`)
	mustPilot(t, err)
	if err := f.s.RecordPilotReceipt(ctx, f.plan.Spec.RunID, receipt); err == nil {
		t.Fatal("invalid evidence accepted")
	}
	value, err := f.s.PilotExecution(ctx, f.plan.Spec.RunID)
	mustPilot(t, err)
	if value.Failure != "" || value.Receipt != nil {
		t.Fatal("audit rollback mutated receipt")
	}
	_, err = f.s.db.Exec(`DROP TRIGGER fail_pilot_audit`)
	mustPilot(t, err)
	if err := f.s.RecordPilotReceipt(ctx, f.plan.Spec.RunID, receipt); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("exit7 accepted: %v", err)
	}
	value, err = f.s.PilotExecution(ctx, f.plan.Spec.RunID)
	mustPilot(t, err)
	if value.Failure != "invalid-receipt" || value.Receipt != nil || !value.Cleaning {
		t.Fatalf("invalid receipt failure not durable: %#v", value)
	}
}

func TestPilotJournalMigration14PreservesLegacyCapacity(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "migration.db")
	now := time.Now().UTC().Add(-time.Minute)
	s, err := Open(ctx, path, Options{Now: func() time.Time { return now }})
	mustPilot(t, err)
	t.Cleanup(func() { mustPilot(t, s.Close()) })
	repo := createRepo(t, s)
	legacy := vmIntent(t, s, repo, "legacy")
	_, err = s.ReserveVMExecution(ctx, legacy)
	mustPilot(t, err)
	mustPilot(t, s.SubmitVMExecution(ctx, legacy.Spec.RunID))
	// Reconstruct precisely the v13 journal shape with an unknown create.
	_, err = s.db.Exec(`DROP TABLE vm_suite_resources; DROP TABLE vm_suite_executions; DROP TABLE vm_capacity_slots;
ALTER TABLE runs DROP COLUMN concurrency_group; DELETE FROM schema_migrations WHERE version>=14;`)
	mustPilot(t, err)
	mustPilot(t, s.Close())
	s, err = Open(ctx, path, Options{Now: func() time.Time { return now }})
	mustPilot(t, err)
	retry, err := s.ClaimNextRun(ctx)
	mustPilot(t, err)
	if retry.ID == legacy.Spec.RunID || retry.Ref != "legacy" || retry.TestedSHA != legacy.Spec.CandidateSHA {
		t.Fatalf("unexpected migrated startup retry: %#v", retry)
	}
	plan := pilotPlan(vmIntent(t, s, repo, "pilot"))
	if _, err := s.ReservePilot(ctx, plan); !errors.Is(err, ErrVMCapacity) {
		t.Fatalf("migration lost legacy obligation: %v", err)
	}
	mustPilot(t, s.RejectVMExecution(ctx, legacy.Spec.RunID))
	mustPilot(t, s.BeginVMCleanup(ctx, legacy.Spec.RunID))
	mustPilot(t, s.CompleteVMCleanup(ctx, legacy.Spec.RunID, vmrunner.VMInstance{}))
	_, err = s.ReservePilot(ctx, plan)
	mustPilot(t, err)
}

func TestPilotJournalSealedPlanAndRestartUIDCannotBeReused(t *testing.T) {
	ctx := context.Background()
	f := newPilotFixture(t)
	f.start(t)
	changed := f.plan
	changed.Spec.SuiteRevision = strings.Repeat("f", 40)
	if _, err := f.s.ReservePilot(ctx, changed); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("suite rebound: %v", err)
	}
	initial := f.create(t, vmrunner.InitialVMResource, "VirtualMachineInstance")
	f.pod(t, "pod-beacon-0", vmrunner.InitialVMResource)
	f.claimRestart(t, f.conductor(t))
	f.clean(t, "pod-beacon-0")
	f.clean(t, vmrunner.InitialVMResource)
	restart := f.intent(vmrunner.RestartVMResource, "VirtualMachineInstance", "")
	mustPilot(t, f.s.AddPilotResource(ctx, f.plan.Spec.RunID, restart))
	mustPilot(t, f.s.SubmitPilotResource(ctx, f.plan.Spec.RunID, restart.Key))
	initial.SpecIdentity = restart.SpecIdentity
	if err := f.s.BindPilotResource(ctx, f.plan.Spec.RunID, restart.Key, initial); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("old restart UID reused: %v", err)
	}
	value, err := f.s.PilotExecution(ctx, f.plan.Spec.RunID)
	mustPilot(t, err)
	if value.Failure != "resource-replaced" {
		t.Fatalf("reused UID did not poison execution: %#v", value)
	}
}

func TestPilotJournalSubmitAuditFailureAndDeadlinePreventExternalCreate(t *testing.T) {
	ctx := context.Background()
	f := newPilotFixture(t)
	f.reserve(t)
	_, err := f.s.db.Exec(`CREATE TRIGGER fail_pilot_submit BEFORE INSERT ON audit_actions WHEN NEW.action='vm.pilot-submitted' BEGIN SELECT RAISE(ABORT,'audit unavailable'); END`)
	mustPilot(t, err)
	if err := f.s.SubmitPilot(ctx, f.plan.Spec.RunID); err == nil {
		t.Fatal("submit survived missing audit")
	}
	value, err := f.s.PilotExecution(ctx, f.plan.Spec.RunID)
	mustPilot(t, err)
	if value.Submitted {
		t.Fatal("audit failure persisted create authority")
	}
	_, err = f.s.db.Exec(`DROP TRIGGER fail_pilot_submit`)
	mustPilot(t, err)
	*f.now = f.now.Add(f.plan.Spec.Deadline)
	if err := f.s.SubmitPilot(ctx, f.plan.Spec.RunID); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("expired pilot submitted: %v", err)
	}
}

func TestPilotJournalRestartCASAndAcknowledgementReplay(t *testing.T) {
	ctx := context.Background()
	f := newPilotFixture(t)
	f.start(t)
	f.create(t, vmrunner.InitialVMResource, "VirtualMachineInstance")
	f.pod(t, "pod-beacon-0", vmrunner.InitialVMResource)
	attempt := f.conductor(t)
	request := f.claimRestart(t, attempt)
	claimed, err := f.s.ClaimPilotRestart(ctx, f.plan.Spec.RunID, request)
	mustPilot(t, err)
	if claimed {
		t.Fatal("duplicate delivery authorized second restart")
	}
	if err := f.s.CompletePilotRestart(ctx, f.plan.Spec.RunID, f.restartResponse(request)); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("restart ack preceded old cleanup and new UID: %v", err)
	}
	f.clean(t, "pod-beacon-0")
	f.clean(t, vmrunner.InitialVMResource)
	f.create(t, vmrunner.RestartVMResource, "VirtualMachineInstance")
	f.pod(t, "pod-beacon-1", vmrunner.RestartVMResource)
	response := f.restartResponse(request)
	mustPilot(t, f.s.CompletePilotRestart(ctx, f.plan.Spec.RunID, response))
	claimed, err = f.s.ClaimPilotRestart(ctx, f.plan.Spec.RunID, request)
	mustPilot(t, err)
	if claimed {
		t.Fatal("acknowledged operation authorized second restart")
	}
	value, err := f.s.PilotExecution(ctx, f.plan.Spec.RunID)
	mustPilot(t, err)
	if value.Restart == nil || value.Restart.Response == nil || *value.Restart.Response != response {
		t.Fatalf("durable restart ack differs: %#v", value.Restart)
	}
	changed := response
	changed.Endpoint.Address = "10.42.1.11:443"
	if err := f.s.CompletePilotRestart(ctx, f.plan.Spec.RunID, changed); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("restart result rebound: %v", err)
	}
	value, err = f.s.PilotExecution(ctx, f.plan.Spec.RunID)
	mustPilot(t, err)
	if value.Failure != "invalid-receipt" {
		t.Fatal("changed acknowledged result did not poison evidence")
	}
}
