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
		{"no observation", nil, nil, "in line"},
		{"unknown state", &model.RunScheduling{State: "unavailable"}, nil, "in line"},
		{"YAML group blocked", waiting(model.AdmissionObservation{Group: "e2e", GroupLimit: 2, GroupBlocked: true}), nil, "group e2e busy"},
		{"YAML group available", waiting(model.AdmissionObservation{Group: "e2e", GroupLimit: 2, Size: "M"}), nil, "in line"},
		{"next in line", &model.RunScheduling{State: "queued", QueuePosition: 1}, nil, "next in line"},
		{"third in line", &model.RunScheduling{State: "queued", QueuePosition: 3}, nil, "position 3"},
		{"scoped viewer, no position", &model.RunScheduling{State: "queued"}, nil, "in line"},
		{"in line behind the head", &model.RunScheduling{State: "queued", QueuePosition: 2}, head, "behind org/app/main"},
		{"preparing", &model.RunScheduling{State: "preparing"}, nil, "starting"},
		{"preflight", &model.RunScheduling{State: "preflight"}, nil, "retired execution preflight"},
		{"admitted, not submitted", &model.RunScheduling{State: "waiting", Admission: &model.AdmissionObservation{State: "admitted"}}, nil, "starting"},
		{"no reservation known", &model.RunScheduling{State: "waiting"}, nil, "in line"},
		{"CPU budget", waiting(model.AdmissionObservation{NumericBudget: budget, DeclaredResources: &model.DeclaredResources{CPUMilli: 16000, MemoryBytes: 1 << 30}, HeadOfQueue: true, BlockedBy: &model.AdmissionBlocker{Backfilling: true}}), nil,
			"next in line"},
		{"memory budget", waiting(model.AdmissionObservation{NumericBudget: budget, DeclaredResources: &model.DeclaredResources{CPUMilli: 1000, MemoryBytes: 12 << 30}}), nil,
			"in line"},
		{"host IO pressure", waiting(model.AdmissionObservation{Pressure: &model.PressureDeferral{Resource: "io", Avg60: 42.4}}), nil, "host IO busy"},
		{"host pressure, scoped", waiting(model.AdmissionObservation{Pressure: &model.PressureDeferral{Resource: "cpu+io", Redacted: true}}), nil, "host IO busy"},
		{"avg10 gate", waiting(model.AdmissionObservation{Pressure: &model.PressureDeferral{Resource: "cpu", Avg10Gate: true, CPUAvg10: 45.25, IOAvg10: 30.6}}), nil, "host CPU busy"},
		{"run ceiling", waiting(model.AdmissionObservation{Runs: 10, MaxRuns: 10, DeclaredResources: &model.DeclaredResources{CPUMilli: 1000}, HeadOfQueue: true, BlockedBy: &model.AdmissionBlocker{RunCeiling: true}}), nil, "10 of 10 runs running"},
		{"run ceiling, scoped", waiting(model.AdmissionObservation{BlockedBy: &model.AdmissionBlocker{RunCeiling: true, Repo: "org/app", Ref: "main"}}), nil, "behind org/app/main"},
		{"behind a head held by pressure", waiting(model.AdmissionObservation{BlockedBy: &model.AdmissionBlocker{Repo: "org/app", Ref: "main", Pressure: &model.PressureDeferral{Resource: "io", Avg60: 42}}}), nil, "host IO busy"},
		{"concurrency group held", waiting(model.AdmissionObservation{Group: "deploy", GroupHolder: "0123456789abcdef0123456789abcdef"}), nil, "group deploy busy"},
		{"concurrency group ahead", waiting(model.AdmissionObservation{Group: "deploy", GroupAhead: "fedcba9876543210fedcba9876543210"}), nil, "group deploy busy"},
		{"disk floor", waiting(model.AdmissionObservation{DiskFloor: &model.DiskFloorObservation{BelowFloor: true, FreeBytes: 512 << 20, FloorBytes: 2 << 30}}), nil, "waiting for disk space (512 MiB free)"},
		{"weight capacity", waiting(model.AdmissionObservation{Size: "M", Weight: 2, Used: 6, Capacity: 8}), nil, "in line"},
		{"behind the head", waiting(model.AdmissionObservation{Size: "S", Weight: 1, Used: 6, Capacity: 8, BlockedBy: head}), nil, "behind org/app/main"},
		{"scoped capacity", waiting(model.AdmissionObservation{}), nil, "in line"},
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
