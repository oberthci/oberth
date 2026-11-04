package service

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

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
	gate := newWeightedAdmission(10)
	gate.diskFloor = &diskFloorCheck{path: t.TempDir(), floorBytes: 1 << 62, cacheTTL: 5 * time.Second}
	reservation := gate.reserve(1)
	defer reservation.cancel()
	gate.mu.Lock()
	reservation.size, reservation.weight, reservation.prepared = "M", 2, true
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
			gate := newWorkflowAdmissionForJobs(4)
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
				if gate.spareWeightForClaim(0) {
					t.Fatal("below-floor gate claimed spare capacity")
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
	gate := newWorkflowAdmissionForJobs(4)
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
			WorkspaceRoot: f.root + "/./floor-work", MaxConcurrent: 4,
			AdmissionDiskFloorBytes: floor,
		})
		if err != nil {
			t.Fatal(err)
		}
		if !scheduler.admission.workflowOnly {
			t.Fatal("disk guard replaced the live workflow gate")
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
