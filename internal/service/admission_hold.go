package service

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
)

type admissionHoldStore interface {
	AdmissionHold(context.Context) (model.AdmissionHold, error)
	SetAdmissionHold(context.Context, string, model.AdmissionHoldRequest) (model.AdmissionHold, error)
}

// The gate lock protects grants only. Durable writes and their audit run
// outside it; holdMu orders changes, including their readback after an error.
func (scheduler *Scheduler) SetAdmissionHold(ctx context.Context, actor string, request model.AdmissionHoldRequest) (model.AdmissionHold, error) {
	if err := scheduler.requireMutation(ctx); err != nil {
		return model.AdmissionHold{}, err
	}
	state, ok := scheduler.store.(admissionHoldStore)
	if !ok {
		return model.AdmissionHold{}, refuse(ErrUnavailable, "admission hold storage")
	}
	scheduler.holdMu.Lock()
	defer scheduler.holdMu.Unlock()
	gate := scheduler.admission
	gate.mu.Lock()
	gate.holdUpdating = true
	gate.mu.Unlock()
	writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	hold, err := state.SetAdmissionHold(writeCtx, actor, request)
	cancel()
	// A lost commit acknowledgement must not restore an older admission rule.
	// Read back with an independent bounded context before releasing the fence.
	if err != nil {
		readCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		current, readErr := state.AdmissionHold(readCtx)
		stop()
		if readErr == nil {
			hold = current
		}
		gate.mu.Lock()
		if readErr == nil {
			gate.adminHold = hold
		}
		gate.holdUpdating = false
		gate.grantLocked()
		gate.mu.Unlock()
		scheduler.signalQueue()
		return model.AdmissionHold{}, err
	}
	gate.mu.Lock()
	gate.adminHold = hold
	gate.holdUpdating = false
	gate.grantLocked()
	snapshot := hold.Snapshot(gate.clock())
	gate.mu.Unlock()
	scheduler.signalQueue()
	return snapshot, nil
}

func (scheduler *Scheduler) AdmissionHold() model.AdmissionHold {
	gate := scheduler.admission
	gate.mu.Lock()
	defer gate.mu.Unlock()
	return gate.adminHold.Snapshot(gate.clock())
}
func (gate *weightedAdmission) adminHeldLocked(r *admissionReservation) bool {
	if r.granted {
		return false
	}
	if gate.holdUpdating {
		return true
	}
	run := model.Run{RefKind: r.refKind, Release: r.pressureRelease}
	if r.pressurePromotion {
		run.Trigger = "promotion"
	}
	return gate.adminHold.Blocks(run, gate.clock())
}
func (gate *weightedAdmission) dispatchParked() (held, other int) {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	for _, r := range gate.waiters {
		if gate.adminHeldLocked(r) {
			held++
			continue
		}
		if holder, ahead := gate.groupBlockerLocked(r); r.prepared && (holder != "" || ahead != "") {
			other++
		}
	}
	return
}

func (service *API) admissionHoldTool(ctx context.Context, actor api.Actor, raw json.RawMessage) (any, error) {
	if !actor.Admin {
		return nil, refuse(ErrForbidden, "admission hold requires an admin uplink")
	}
	if err := requireUnscoped(actor, "admission_hold"); err != nil {
		return nil, err
	}
	var args struct {
		On       *bool    `json:"on"`
		Reason   string   `json:"reason"`
		Allow    []string `json:"allow"`
		Duration string   `json:"duration"`
	}
	if err := decodeTool(raw, &args); err != nil {
		return nil, err
	}
	if args.On == nil {
		return nil, fmt.Errorf("%w: on is required", ErrInvalidInput)
	}
	request := model.AdmissionHoldRequest{On: *args.On, Reason: args.Reason, Allow: args.Allow}
	if args.Duration != "" {
		duration, err := time.ParseDuration(args.Duration)
		if err != nil || duration <= 0 || duration > model.MaxAdmissionHoldDuration {
			return nil, fmt.Errorf("%w: duration must be positive and at most 12h", ErrInvalidInput)
		}
		request.Duration = duration
	}
	holder, ok := service.enqueues.(interface {
		SetAdmissionHold(context.Context, string, model.AdmissionHoldRequest) (model.AdmissionHold, error)
	})
	if !ok {
		return nil, refuse(ErrUnavailable, "admission hold scheduler")
	}
	return holder.SetAdmissionHold(ctx, actor.Identity, request)
}
func (service *API) admissionHoldForRun(run model.Run) (model.AdmissionHold, bool) {
	// A grant is already owned, even before its StartedAt receipt is persisted.
	if observer, ok := service.enqueues.(interface {
		AdmissionSnapshot(int64) *model.AdmissionObservation
	}); ok {
		if state := observer.AdmissionSnapshot(run.QueueSequence); state != nil && state.State == "admitted" {
			return model.AdmissionHold{}, false
		}
	}
	observer, ok := service.enqueues.(interface{ AdmissionHold() model.AdmissionHold })
	if !ok || run.StartedAt != nil {
		return model.AdmissionHold{}, false
	}
	hold := observer.AdmissionHold()
	return hold, hold.Blocks(run, service.schedulingClock().Now())
}
