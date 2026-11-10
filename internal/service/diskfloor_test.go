package service

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
)

func TestSchedulerResumesRunAfterDiskFloorClears(t *testing.T) {
	f := newControlFixture(t)
	jobs := newConcurrentJobs("", 2)
	scheduler, err := NewScheduler(SchedulerConfig{
		Store: f.store, Git: f.git, Logs: f.logs, Jobs: jobs, ReleaseJobs: jobs,
		Auditor: f.store, Signals: f.signals, WorkspaceRoot: filepath.Join(f.root, "floor-work"),
		AdmissionDiskFloorBytes: 2048,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.scheduler = scheduler
	check := scheduler.admission.diskFloor
	if check == nil {
		t.Fatal("scheduler did not wire the configured disk floor")
	}
	// Control only the filesystem sample, without filling the host disk or
	// changing the configured floor. Admission and dispatch run normally.
	check.mu.Lock()
	check.cachedFree, check.cacheTime, check.cacheTTL = 1024, time.Now(), time.Hour
	check.mu.Unlock()
	ticks := make(chan time.Time, 1)
	scheduler.dispatchTicks = ticks
	run, err := scheduler.EnqueueCI(t.Context(), CIRequest{
		EventID: "disk-recovery", Repository: f.repo, Branch: "feature/disk-recovery",
		SHA: strings.Repeat("a", 40), Actor: "agent@host",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- scheduler.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(testStartTimeout(5 * time.Second)):
			t.Error("scheduler did not stop")
		}
	})
	service := f.api(t)
	service.admission = scheduler
	deadline := time.NewTimer(testStartTimeout(5 * time.Second))
	defer deadline.Stop()
	for {
		value, err := service.Run(t.Context(), api.Actor{Identity: "agent@host"}, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		detail := value.(RunDetailResponse)
		if detail.Scheduling != nil && detail.Scheduling.State == "waiting" && detail.Scheduling.Admission != nil && detail.Scheduling.Admission.DiskFloor != nil {
			if detail.Run.DisplayStatus != "queued" || detail.Run.StartedAt != nil || detail.Run.DisplayReason != "waiting for disk space (1 KiB free)" || !detail.Scheduling.Admission.DiskFloor.BelowFloor {
				t.Fatalf("disk hold lost its queued reason: %+v, %+v", detail.Run, detail.Scheduling)
			}
			break
		}
		select {
		case <-jobs.admitted:
			t.Fatal("run submitted below the disk floor")
		case <-deadline.C:
			t.Fatalf("disk hold was not visible: %+v, %+v", detail.Run, detail.Scheduling)
		case <-time.After(10 * time.Millisecond):
		}
	}
	select {
	case <-jobs.admitted:
		t.Fatal("held run reached workflow creation")
	default:
	}
	if state := scheduler.AdmissionState(); state.Runs != 0 {
		t.Fatalf("disk hold consumed a grant: %+v", state)
	}
	check.mu.Lock()
	check.cachedFree = check.floorBytes // Exactly at the floor is sufficient.
	check.mu.Unlock()
	// Wake the regular dispatch poll; the test never grants a reservation.
	ticks <- time.Now()
	select {
	case id := <-jobs.admitted:
		if id != run.ID {
			t.Fatalf("admitted %s, want %s", id, run.ID)
		}
	case <-time.After(testStartTimeout(5 * time.Second)):
		t.Fatal("disk recovery did not submit the waiting run")
	}
	close(jobs.release)
	finished := waitForFixtureRunStatus(t, f, run.ID, model.RunPassed)
	if finished.StartedAt == nil {
		t.Fatal("submitted run has no start time")
	}
	jobs.mu.Lock()
	created := append([]string(nil), jobs.created...)
	jobs.mu.Unlock()
	if len(created) != 1 || created[0] != run.ID {
		t.Fatalf("disk recovery submitted workflow more than once: %v", created)
	}
}

func TestDiskFloorNil(t *testing.T) {
	var check *diskFloorCheck
	if !check.freeAboveFloor() {
		t.Fatal("nil check should always return true")
	}
	if obs := check.observation(); obs != nil {
		t.Fatal("nil check should return nil observation")
	}
}

func TestDiskFloorAdmissionHold(t *testing.T) {
	gate := newAdmissionGate()
	gate.diskFloor = &diskFloorCheck{path: t.TempDir(), floorBytes: 1 << 62, cacheTTL: 5 * time.Second}
	reservation := gate.reserve(1)
	defer reservation.cancel()
	gate.mu.Lock()
	reservation.prepared = true
	can := gate.canGrantLocked(reservation)
	gate.mu.Unlock()
	if can {
		t.Fatal("admission should be held when disk is below floor")
	}
}

func TestWorkflowAdmissionDiskFloorHoldAndGrant(t *testing.T) {
	for _, tc := range []struct {
		name  string
		floor int64
		allow bool
	}{{"below", 1 << 62, false}, {"above", 1, true}, {"disabled", 0, true}} {
		t.Run(tc.name, func(t *testing.T) {
			gate := newAdmissionGate()
			gate.diskFloor = &diskFloorCheck{path: t.TempDir(), floorBytes: tc.floor, cacheTTL: 5 * time.Second}
			reservation := gate.reserveRun(model.Run{ID: "waiting", QueueSequence: 1})
			defer reservation.cancel()
			gate.mu.Lock()
			reservation.prepared = true
			can := gate.canGrantLocked(reservation)
			blocker := gate.blockerLocked(reservation)
			gate.grantLocked()
			gotGrant, runs := reservation.granted, gate.runs
			gate.mu.Unlock()
			if can != tc.allow || gotGrant != tc.allow || blocker.DiskFloor == tc.allow {
				t.Fatalf("allow=%t: can=%t granted=%t blocker=%+v", tc.allow, can, gotGrant, blocker)
			}
			if gotGrant && runs != 1 || !gotGrant && runs != 0 {
				t.Fatalf("grant count changed while held: %d", runs)
			}
			if !tc.allow {
				if gate.spareSlotForClaim() {
					t.Fatal("a second claim passed the run waiting below the floor")
				}
				observation := (&Scheduler{admission: gate}).AdmissionSnapshot(1)
				if observation == nil || observation.State != "waiting" || observation.DiskFloor == nil || !observation.DiskFloor.BelowFloor || !strings.HasPrefix(waitingReason(observation), "waiting for disk space (") {
					t.Fatalf("existing disk-space reason lost: %+v", observation)
				}
			}
		})
	}
}

func TestWorkflowAdmissionDiskFloorPreservesGrantedRun(t *testing.T) {
	gate := newAdmissionGate()
	// Cached samples make threshold changes deterministic without filling disk.
	gate.diskFloor = &diskFloorCheck{floorBytes: 512, cachedFree: 1024, cacheTime: time.Now(), cacheTTL: time.Hour}
	first := gate.reserveRun(model.Run{ID: "running", QueueSequence: 1})
	defer first.cancel()
	gate.mu.Lock()
	first.prepared = true
	gate.grantLocked()
	firstGranted, grantedAt := first.granted, first.grantedAt
	gate.diskFloor = &diskFloorCheck{floorBytes: 2048, cachedFree: 1024, cacheTime: time.Now(), cacheTTL: time.Hour}
	gate.mu.Unlock()
	if !firstGranted {
		t.Fatal("above-floor workflow did not get its initial grant")
	}
	second := gate.reserveRun(model.Run{ID: "waiting", QueueSequence: 2})
	defer second.cancel()
	gate.mu.Lock()
	second.prepared = true
	gate.grantLocked()
	unchanged := first.granted && !first.released && !first.canceled && first.grantedAt.Equal(grantedAt)
	secondGranted, runs := second.granted, gate.runs
	blocker := gate.blockerLocked(second)
	gate.mu.Unlock()
	if !unchanged || secondGranted || runs != 1 || !blocker.DiskFloor {
		t.Fatalf("floor changed existing grant or admitted new work: unchanged=%t second=%t runs=%d blocker=%+v", unchanged, secondGranted, runs, blocker)
	}
	select {
	case <-first.ready:
	default:
		t.Fatal("existing granted run lost its readiness signal")
	}
	select {
	case <-second.ready:
		t.Fatal("held workflow received a readiness signal")
	default:
	}
	gate.mu.Lock()
	gate.diskFloor = &diskFloorCheck{floorBytes: 512, cachedFree: 1024, cacheTime: time.Now(), cacheTTL: time.Hour}
	gate.grantLocked()
	resumed := first.granted && first.grantedAt.Equal(grantedAt) && second.granted && gate.runs == 2
	gate.mu.Unlock()
	if !resumed {
		t.Fatal("disk recovery did not admit the waiting workflow exactly once")
	}
}

func TestNewSchedulerWiresDiskFloor(t *testing.T) {
	f := newControlFixture(t)
	for _, floor := range []int64{0, 4096} {
		scheduler, err := NewScheduler(SchedulerConfig{
			Store: f.store, Git: f.git, Logs: f.logs, Jobs: f.jobs, Auditor: f.store,
			WorkspaceRoot: f.root + "/./floor-work", AdmissionDiskFloorBytes: floor,
		})
		if err != nil {
			t.Fatal(err)
		}
		check := scheduler.admission.diskFloor
		if floor == 0 {
			if check != nil {
				t.Fatal("disabled floor was wired")
			}
			continue
		}
		if check == nil || check.floorBytes != floor || check.path != filepath.Join(f.root, "floor-work") || check.cacheTTL != 5*time.Second {
			t.Fatalf("scheduler lost configured data-root floor: %+v", check)
		}
	}
}
