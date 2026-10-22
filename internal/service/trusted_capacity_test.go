package service

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/vmrunner"
	"github.com/oberthci/oberth/pkg/periapsis"
)

type capacityFixture struct {
	*protectedFixture
	calls        int
	refusals     int
	requirements []vmrunner.Requirement
	complete     bool
	supportErr   error
}

func (p *capacityFixture) Supports(contract vmrunner.Contract) error {
	if p.supportErr != nil {
		return p.supportErr
	}
	return p.protectedFixture.Supports(contract)
}

func (p *capacityFixture) Execute(ctx context.Context, job JobRequest, requirement vmrunner.Requirement, output io.Writer) error {
	p.calls++
	p.requirements = append(p.requirements, requirement)
	if p.calls <= max(1, p.refusals) {
		return store.ErrVMCapacity
	}
	if p.complete {
		return p.protectedFixture.Execute(ctx, job, requirement, output)
	}
	return nil
}

func TestTrustedCapacityParkReleasesResourcesAndPreservesOrder(t *testing.T) {
	gate := newWeightedAdmissionForJobs(1)
	first := gate.reserve(1)
	release := acquireRequestWithinBudget(t, first, groupRequest("vm", 1, periapsis.M, "shared"))
	defer release()
	if err := first.parkVM(); err != nil {
		t.Fatal(err)
	}
	if used, runs := gateUsage(gate); used != 0 || runs != 0 {
		t.Fatalf("park retained resources: %d %d", used, runs)
	}
	scheduler := &Scheduler{admission: gate, maxConcurrent: 1}
	if !scheduler.dispatchSlotAvailable(1) || scheduler.dispatchSlotAvailable(2) {
		t.Fatal("parked dispatch exemption unbounded or missing")
	}
	ordinary := gate.reserve(2)
	releaseOther := acquireRequestWithinBudget(t, ordinary, groupRequest("ordinary", 1, periapsis.M, "shared"))
	olderReady := make(chan func(), 1)
	go acquireRequestForTest(first, groupRequest("vm", 1, periapsis.M, "shared"), olderReady)
	awaitWaiting(t, first)
	later := gate.reserve(3)
	laterReady := make(chan func(), 1)
	go acquireRequestForTest(later, groupRequest("later", 1, periapsis.M, "shared"), laterReady)
	awaitWaiting(t, later)
	releaseOther()
	awaitAcquired(t, olderReady)()
	awaitAcquired(t, laterReady)()
}

func TestTrustedCapacityRetryBoundaries(t *testing.T) {
	for _, mode := range []string{"retry", "cancel", "supersede", "mutation blocked", "existing journal", "policy changed", "restart"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			f := newControlFixture(t)
			raw := []byte("version: 2\nprofile: kvm-suite-amd64-v1\nsuite_sha256: " + strings.Repeat("a", 64) + "\nguest_sha256: " + strings.Repeat("b", 64) + "\nartifacts: [build/program]\n")
			git := &policyTestGit{controlGit: f.git, candidate: raw, baseline: raw}
			f.scheduler.git = git
			executor := &capacityFixture{protectedFixture: &protectedFixture{store: f.store}}
			f.scheduler.trusted = executor
			_, err := f.store.EnqueueRun(ctx, model.RunSpec{RepoID: f.repo.ID, RefKind: model.RefBranch, Ref: "feature/capacity", SHA: strings.Repeat("c", 40), TestedSHA: strings.Repeat("c", 40), Actor: "agent", Trigger: "branch"})
			if err != nil {
				t.Fatal(err)
			}
			run, err := f.store.ClaimNextRun(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.store.MarkRunAdmitted(ctx, run.ID); err != nil {
				t.Fatal(err)
			}
			requirement, err := f.scheduler.trustedRequirement(ctx, f.repo, run.ID, run.TestedSHA)
			if err != nil {
				t.Fatal(err)
			}
			job := JobRequest{Run: run, Repository: f.repo, Size: periapsis.M, ConcurrencyGroup: "group", UpstreamName: "codeberg", UpstreamOrg: "acme"}
			reservation := f.scheduler.admission.reserve(1)
			defer acquireRequestWithinBudget(t, reservation, groupRequest(run.ID, f.repo.ID, job.Size, job.ConcurrencyGroup))()
			if mode == "existing journal" {
				executor.omitCleanup = true
				if err := executor.protectedFixture.Execute(ctx, job, requirement, io.Discard); err == nil {
					t.Fatal("expected pending cleanup fixture")
				}
			}
			waited := false
			f.scheduler.capacityWait = func(context.Context) error {
				waited = true
				parked, err := f.store.Run(ctx, run.ID)
				if err != nil || parked.StartedAt != nil || displayRunStatus(parked) != "queued" {
					t.Fatalf("parked run remains visible as admitted: %+v, %v", parked, err)
				}
				reader := f.api(t)
				reader.admission = f.scheduler
				listed, err := reader.Runs(ctx, api.Actor{Identity: "agent"}, api.RunFilter{})
				if err != nil {
					t.Fatal(err)
				}
				if entries := listed.([]model.Run); len(entries) != 1 || entries[0].DisplayStatus != "queued" || entries[0].StartedAt != nil || !strings.Contains(entries[0].DisplayReason, "trusted VM capacity") {
					t.Fatalf("parked run list presentation = %+v", entries)
				}
				if used, runs := gateUsage(f.scheduler.admission); used != 0 || runs != 0 {
					t.Fatal("waiting with resources held")
				}
				if _, err := f.store.KVMPreflight(ctx, run.ID); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("park wrote intent: %v", err)
				}
				switch mode {
				case "cancel":
					return context.Canceled
				case "supersede":
					_, err := f.store.EnqueueRun(ctx, model.RunSpec{RepoID: f.repo.ID, RefKind: model.RefBranch, Ref: run.Ref, SHA: strings.Repeat("d", 40), Actor: "agent", Trigger: "branch"})
					return err
				case "policy changed":
					git.candidate = []byte(strings.ReplaceAll(string(raw), "build/program", "build/changed"))
					git.baseline = git.candidate
				case "restart":
					restarted, err := store.Open(ctx, filepath.Join(f.root, "oberth.sqlite"), store.Options{})
					if err != nil {
						return err
					}
					defer restarted.Close()
				}
				return nil
			}
			require := func(context.Context) error {
				if mode == "mutation blocked" {
					return errMutationBlocked
				}
				return nil
			}
			stopped, err := f.scheduler.executeTrustedWithCapacity(ctx, job, requirement, io.Discard, reservation, require)
			switch mode {
			case "retry":
				if err != nil || stopped || executor.calls != 2 {
					t.Fatalf("retry: %v %v calls%d", stopped, err, executor.calls)
				}
				readmitted, lookupErr := f.store.Run(ctx, run.ID)
				if lookupErr != nil || readmitted.StartedAt == nil || displayRunStatus(readmitted) != "running" {
					t.Fatalf("reacquired run lacks admission time: %+v, %v", readmitted, lookupErr)
				}
				if len(git.reads) < 4 {
					t.Fatal("retained policy not refreshed")
				}
			case "supersede", "restart":
				if err != nil || !stopped || executor.calls != 1 {
					t.Fatalf("stopped: %v %v calls%d", stopped, err, executor.calls)
				}
			default:
				if err == nil || executor.calls != 1 {
					t.Fatalf("unsafe retry: %v calls%d", err, executor.calls)
				}
			}
			if mode == "existing journal" && waited {
				t.Fatal("existing intent parked")
			}
			if len(f.jobs.createdCI)+len(f.jobs.createdReleases)+len(f.git.syncedBranches)+len(f.git.syncedTags) != 0 {
				t.Fatal("capacity wait delivered or published")
			}
		})
	}
}

func TestTrustedCapacityRetryDeliversReleaseOnlyAfterReceipt(t *testing.T) {
	ctx := context.Background()
	f := newControlFixture(t, JobResult{Status: model.RunPassed, Phase: "Succeeded"})
	raw := []byte("version: 2\nprofile: kvm-suite-amd64-v1\nsuite_sha256: " + strings.Repeat("a", 64) + "\nguest_sha256: " + strings.Repeat("b", 64) + "\nartifacts: [build/program]\n")
	f.scheduler.git = &policyTestGit{controlGit: f.git, candidate: raw, baseline: raw}
	executor := &capacityFixture{protectedFixture: &protectedFixture{store: f.store}, complete: true}
	f.scheduler.trusted = executor
	f.scheduler.capacityWait = func(context.Context) error {
		if len(f.jobs.createdReleases) != 0 {
			t.Fatal("credentials released while capacity unavailable")
		}
		return nil
	}
	queued, err := f.scheduler.AdmitRelease(ctx, ReleaseRequest{EventID: "capacity-release", Repository: f.repo, Tag: "v1.0.0", ObjectSHA: strings.Repeat("d", 40), CommitSHA: strings.Repeat("c", 40), Actor: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	if err = f.scheduler.ProcessNext(ctx); err != nil {
		t.Fatal(err)
	}
	run, err := f.store.Run(ctx, queued.ID)
	if err != nil || run.Status != model.RunPassed || len(f.jobs.createdReleases) != 1 || executor.calls != 2 {
		t.Fatalf("retry did not complete release: %#v %v calls%d", run, err, executor.calls)
	}
	if used, runs := gateUsage(f.scheduler.admission); used != 0 || runs != 0 {
		t.Fatal("reacquired grant leaked")
	}
}

func TestTrustedCapacityRotationAuditsOnceAcrossRetries(t *testing.T) {
	ctx := context.Background()
	f := newControlFixture(t)
	baseline := "version: 2\nprofile: kvm-suite-amd64-v1\nsuite_sha256: " + strings.Repeat("a", 64) + "\nguest_sha256: " + strings.Repeat("b", 64) + "\nartifacts: [build/program]\n"
	candidate := strings.ReplaceAll(strings.ReplaceAll(baseline, strings.Repeat("a", 64), strings.Repeat("c", 64)), strings.Repeat("b", 64), strings.Repeat("d", 64))
	git := &policyTestGit{controlGit: f.git, candidate: []byte(candidate), baseline: []byte(baseline)}
	f.scheduler.git = git
	executor := &capacityFixture{protectedFixture: &protectedFixture{store: f.store}, refusals: 3}
	f.scheduler.trusted = executor
	_, err := f.store.EnqueueRun(ctx, model.RunSpec{RepoID: f.repo.ID, RefKind: model.RefBranch, Ref: "feature/rotation", SHA: strings.Repeat("e", 40), TestedSHA: strings.Repeat("e", 40), Actor: "agent", Trigger: "branch"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := f.store.ClaimNextRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.MarkRunAdmitted(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	requirement, err := f.scheduler.trustedRequirement(ctx, f.repo, run.ID, run.TestedSHA)
	if err != nil || requirement.RotatedFrom == nil {
		t.Fatalf("rotation admission: %+v, %v", requirement, err)
	}
	assertOneAudit := func() {
		t.Helper()
		actions, _, err := f.store.ListAuditActions(ctx, model.AuditListFilter{ActionPrefix: "trusted.pin_rotation.authorized", RepoID: f.repo.ID})
		if err != nil || len(actions) != 1 {
			t.Fatalf("capacity-path rotation audits = %d, want 1: %v", len(actions), err)
		}
	}
	waits := 0
	f.scheduler.capacityWait = func(context.Context) error {
		waits++
		assertOneAudit()
		return nil
	}
	job := JobRequest{Run: run, Repository: f.repo, Size: periapsis.M, ConcurrencyGroup: "group"}
	reservation := f.scheduler.admission.reserve(1)
	defer acquireRequestWithinBudget(t, reservation, groupRequest(run.ID, f.repo.ID, job.Size, job.ConcurrencyGroup))()
	stopped, err := f.scheduler.executeTrustedWithCapacity(ctx, job, requirement, io.Discard, reservation, func(context.Context) error { return nil })
	if err != nil || stopped || waits != 3 || executor.calls != 4 {
		t.Fatalf("capacity retries: stopped=%v err=%v waits=%d calls=%d", stopped, err, waits, executor.calls)
	}
	assertOneAudit()
	if len(git.reads) != 8 {
		t.Fatalf("policy reads = %d, want admission plus three fresh candidate/default reads", len(git.reads))
	}
	// Revocation during another wait is a new, audited refusal, not authority
	// inherited from the successful checks above.
	executor.calls, executor.refusals = 0, 1
	f.scheduler.capacityWait = func(context.Context) error {
		executor.supportErr = errors.New("recipe removed")
		return nil
	}
	if _, err := f.scheduler.executeTrustedWithCapacity(ctx, job, requirement, io.Discard, reservation, func(context.Context) error { return nil }); err == nil || !strings.Contains(err.Error(), "recipe removed") || executor.calls != 1 {
		t.Fatalf("capacity revocation was bypassed: calls=%d err=%v", executor.calls, err)
	}
	refused, _, err := f.store.ListAuditActions(ctx, model.AuditListFilter{ActionPrefix: "trusted.pin_rotation.refused", RepoID: f.repo.ID})
	if err != nil || len(refused) != 1 {
		t.Fatalf("capacity revocation audit count=%d err=%v", len(refused), err)
	}
}

func TestTrustedCapacityAuditsRotationThatAppearsWhileParked(t *testing.T) {
	ctx := context.Background()
	f := newControlFixture(t)
	pins := "version: 2\nprofile: kvm-suite-amd64-v1\nsuite_sha256: " + strings.Repeat("c", 64) + "\nguest_sha256: " + strings.Repeat("d", 64) + "\nartifacts: [build/program]\n"
	older := strings.ReplaceAll(strings.ReplaceAll(pins, strings.Repeat("c", 64), strings.Repeat("a", 64)), strings.Repeat("d", 64), strings.Repeat("b", 64))
	git := &policyTestGit{controlGit: f.git, candidate: []byte(pins), baseline: []byte(pins)}
	f.scheduler.git = git
	executor := &capacityFixture{protectedFixture: &protectedFixture{store: f.store}, refusals: 2}
	f.scheduler.trusted = executor
	_, err := f.store.EnqueueRun(ctx, model.RunSpec{RepoID: f.repo.ID, RefKind: model.RefBranch, Ref: "feature/parked", SHA: strings.Repeat("e", 40), TestedSHA: strings.Repeat("e", 40), Actor: "agent", Trigger: "branch"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := f.store.ClaimNextRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.MarkRunAdmitted(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	requirement, err := f.scheduler.trustedRequirement(ctx, f.repo, run.ID, run.TestedSHA)
	if err != nil || requirement.RotatedFrom != nil {
		t.Fatalf("admission without rotation: %+v, %v", requirement, err)
	}
	audits := func() int {
		t.Helper()
		actions, _, err := f.store.ListAuditActions(ctx, model.AuditListFilter{ActionPrefix: "trusted.pin_rotation.authorized", RepoID: f.repo.ID})
		if err != nil {
			t.Fatal(err)
		}
		return len(actions)
	}
	// The default branch moves to older pins while the run is parked, so the
	// unchanged candidate becomes a rotation that must be audited before it runs.
	waits := 0
	f.scheduler.capacityWait = func(context.Context) error {
		waits++
		git.baseline = []byte(older)
		return nil
	}
	job := JobRequest{Run: run, Repository: f.repo, Size: periapsis.M, ConcurrencyGroup: "group"}
	reservation := f.scheduler.admission.reserve(1)
	defer acquireRequestWithinBudget(t, reservation, groupRequest(run.ID, f.repo.ID, job.Size, job.ConcurrencyGroup))()
	stopped, err := f.scheduler.executeTrustedWithCapacity(ctx, job, requirement, io.Discard, reservation, func(context.Context) error { return nil })
	if err != nil || stopped || waits != 2 || executor.calls != 3 {
		t.Fatalf("capacity retries: stopped=%v err=%v waits=%d calls=%d", stopped, err, waits, executor.calls)
	}
	if got := audits(); got != 1 {
		t.Fatalf("rotation audits after it appeared while parked = %d, want 1", got)
	}
}
