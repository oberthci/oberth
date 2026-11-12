package service

import (
	"testing"

	"github.com/oberthci/oberth/internal/model"
)

// One grey queued status; the reason line says what the run waits for.
func TestQueuedDisplayReasonNamesWhatTheRunWaitsFor(t *testing.T) {
	waiting := func(admission model.AdmissionObservation) *model.RunScheduling {
		admission.State = "waiting"
		return &model.RunScheduling{State: "waiting", Admission: &admission}
	}
	head := &model.AdmissionBlocker{Repo: "org/app", Ref: "main"}
	for _, tc := range []struct {
		name       string
		scheduling *model.RunScheduling
		blocker    *model.AdmissionBlocker
		want       string
	}{
		{"no observation", nil, nil, "in line"},
		{"unknown state", &model.RunScheduling{State: "unavailable"}, nil, "in line"},
		{"next in line", &model.RunScheduling{State: "queued", QueuePosition: 1}, nil, "next in line"},
		{"third in line", &model.RunScheduling{State: "queued", QueuePosition: 3}, nil, "position 3"},
		{"scoped viewer, no position", &model.RunScheduling{State: "queued"}, nil, "in line"},
		{"in line behind the head", &model.RunScheduling{State: "queued", QueuePosition: 2}, head, "behind org/app/main"},
		{"preparing", &model.RunScheduling{State: "preparing"}, nil, "starting"},
		{"preflight", &model.RunScheduling{State: "preflight"}, nil, "retired execution preflight"},
		{"admitted, not submitted", &model.RunScheduling{State: "waiting", Admission: &model.AdmissionObservation{State: "admitted"}}, nil, "starting"},
		{"no reservation known", &model.RunScheduling{State: "waiting"}, nil, "in line"},
		{"head of the queue", waiting(model.AdmissionObservation{HeadOfQueue: true, BlockedBy: &model.AdmissionBlocker{}}), nil,
			"next in line"},
		{"disk floor", waiting(model.AdmissionObservation{}), nil, "waiting for disk space (512 MiB free)"},
		{"runs admitted", waiting(model.AdmissionObservation{Used: 6}), nil, "in line"},
		{"behind the head", waiting(model.AdmissionObservation{Used: 6, BlockedBy: head}), nil, "behind org/app/main"},
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
