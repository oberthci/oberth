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
	requirements []vmrunner.Requirement
	complete     bool
}

func (p *capacityFixture) Execute(ctx context.Context, job JobRequest, requirement vmrunner.Requirement, output io.Writer) error {
	p.calls++
	p.requirements = append(p.requirements, requirement)
	if p.calls == 1 {
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
			requirement, err := f.scheduler.trustedRequirement(ctx, f.repo, run.TestedSHA)
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
				listed, err := f.api(t).Runs(ctx, api.Actor{Identity: "agent"}, api.RunFilter{})
				if err != nil {
					t.Fatal(err)
				}
				if entries := listed.([]model.Run); len(entries) != 1 || entries[0].DisplayStatus != "queued" || entries[0].StartedAt != nil {
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
