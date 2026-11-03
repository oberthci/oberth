package service

import (
	"testing"

	"github.com/oberthci/oberth/internal/model"
)

// One grey queued status; the reason line says what the run waits for.
func TestQueuedDisplayReasonNamesWhatTheRunWaitsFor(t *testing.T) {
	budget := &model.NumericBudget{CPUUsedMilli: 27500, CPUBudgetMilli: 40000, MemoryUsedBytes: 8 << 30, MemoryBudgetBytes: 16 << 30}
	waiting := func(admission model.AdmissionObservation) *model.RunScheduling {
		admission.State = "waiting"
		return &model.RunScheduling{State: "waiting", Admission: &admission}
	}
	head := &model.AdmissionBlocker{Repo: "org/app", Ref: "main", Size: "XL", FullDrain: true}
	for _, tc := range []struct {
		name       string
		scheduling *model.RunScheduling
		blocker    *model.AdmissionBlocker
		want       string
	}{
		{"no observation", nil, nil, "awaiting scheduler"},
		{"next in line", &model.RunScheduling{State: "queued", QueuePosition: 1}, nil, "next in line"},
		{"third in line", &model.RunScheduling{State: "queued", QueuePosition: 3}, nil, "position 3 in line"},
		{"scoped viewer, no position", &model.RunScheduling{State: "queued"}, nil, "in line"},
		{"in line behind the head", &model.RunScheduling{State: "queued", QueuePosition: 2}, head, "position 2 in line, behind org/app/main (needs the whole host)"},
		{"preparing", &model.RunScheduling{State: "preparing"}, nil, "preparing to run"},
		{"preflight", &model.RunScheduling{State: "preflight"}, nil, "KVM preflight running"},
		{"admitted, not submitted", &model.RunScheduling{State: "waiting", Admission: &model.AdmissionObservation{State: "admitted"}}, nil, "admitted, starting the workflow"},
		{"no reservation known", &model.RunScheduling{State: "waiting"}, nil, "admitted, starting the workflow"},
		{"CPU budget", waiting(model.AdmissionObservation{NumericBudget: budget, DeclaredResources: &model.DeclaredResources{CPUMilli: 16000, MemoryBytes: 1 << 30}, HeadOfQueue: true, BlockedBy: &model.AdmissionBlocker{Backfilling: true}}), nil,
			"waiting for CPU budget (12.5 of 40 CPU free); smaller runs run meanwhile"},
		{"memory budget", waiting(model.AdmissionObservation{NumericBudget: budget, DeclaredResources: &model.DeclaredResources{CPUMilli: 1000, MemoryBytes: 12 << 30}}), nil,
			"waiting for memory budget (8 GiB of 16 GiB free)"},
		{"host IO pressure", waiting(model.AdmissionObservation{Pressure: &model.PressureDeferral{Resource: "io", Avg60: 42.4}}), nil, "host IO pressure (42%)"},
		{"host pressure, scoped", waiting(model.AdmissionObservation{Pressure: &model.PressureDeferral{Resource: "cpu+io", Redacted: true}}), nil, "host CPU and IO pressure"},
		{"avg10 gate", waiting(model.AdmissionObservation{Pressure: &model.PressureDeferral{Resource: "cpu", Avg10Gate: true, CPUAvg10: 45.25, IOAvg10: 30.6}}), nil, "host CPU pressure (CPU 45%, IO 31%)"},
		{"run ceiling", waiting(model.AdmissionObservation{Runs: 10, MaxRuns: 10, DeclaredResources: &model.DeclaredResources{CPUMilli: 1000}, HeadOfQueue: true, BlockedBy: &model.AdmissionBlocker{RunCeiling: true}}), nil, "10 runs already running (limit 10)"},
		{"run ceiling, scoped", waiting(model.AdmissionObservation{BlockedBy: &model.AdmissionBlocker{RunCeiling: true, Repo: "org/app", Ref: "main"}}), nil, "waiting for a run slot, behind org/app/main (waiting for a run slot)"},
		{"concurrency group held", waiting(model.AdmissionObservation{Group: "deploy", GroupHolder: "0123456789abcdef0123456789abcdef"}), nil, "concurrency group deploy busy (held by run 0123456789ab)"},
		{"concurrency group ahead", waiting(model.AdmissionObservation{Group: "deploy", GroupAhead: "fedcba9876543210fedcba9876543210"}), nil, "concurrency group deploy busy (run fedcba987654 is ahead)"},
		{"KubeVirt grant", waiting(model.AdmissionObservation{KubeVirtCapacity: true}), nil, "waiting for KubeVirt capacity (repository VM grant)"},
		{"trusted VM", waiting(model.AdmissionObservation{VMCapacity: true}), nil, "waiting for trusted VM capacity"},
		{"disk floor", waiting(model.AdmissionObservation{DiskFloor: &model.DiskFloorObservation{BelowFloor: true, FreeBytes: 512 << 20, FloorBytes: 2 << 30}}), nil, "waiting for disk space (512 MiB free, floor 2 GiB)"},
		{"weight capacity", waiting(model.AdmissionObservation{Size: "M", Weight: 2, Used: 6, Capacity: 8}), nil, "waiting for capacity (size M, 6 of 8 weight used)"},
		{"behind the head", waiting(model.AdmissionObservation{Size: "S", Weight: 1, Used: 6, Capacity: 8, BlockedBy: head}), nil, "waiting for capacity (size S, 6 of 8 weight used), behind org/app/main (needs the whole host)"},
		{"scoped capacity", waiting(model.AdmissionObservation{}), nil, "waiting for capacity"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := queuedDisplayReason(tc.scheduling, tc.blocker); got != tc.want {
				t.Fatalf("reason = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestApplyQueuedDisplayOnlyTouchesQueuedRuns(t *testing.T) {
	scheduling := &model.RunScheduling{State: "queued", QueuePosition: 1, Message: "Awaiting scheduler claim."}
	queued := model.Run{DisplayStatus: model.DisplayQueued, DisplayReason: "Priority normal: ordinary run."}
	applyQueuedDisplay(&queued, scheduling, "")
	if queued.DisplayReason != "next in line" {
		t.Fatalf("queued reason = %q", queued.DisplayReason)
	}
	applyQueuedDisplay(&queued, scheduling, "host IO pressure")
	if queued.DisplayReason != "host IO pressure" {
		t.Fatalf("observed reason lost: %q", queued.DisplayReason)
	}
	running := model.Run{DisplayStatus: model.DisplayRunning, DisplayReason: "upstream publication pending"}
	applyQueuedDisplay(&running, scheduling, "next in line")
	if running.DisplayReason != "upstream publication pending" {
		t.Fatalf("running reason changed: %q", running.DisplayReason)
	}
}
