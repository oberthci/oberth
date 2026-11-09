package service

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/pkg/pipeline"
)

func TestAdmissionHoldToolKeepsOrdinaryWorkQueued(t *testing.T) {
	f := newControlFixture(t)
	control := f.api(t)
	actor := api.Actor{Identity: "operator", Admin: true}
	value, err := control.CallTool(t.Context(), actor, "admission_hold", json.RawMessage(`{"on":true,"reason":"release qualification"}`))
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(value)
	if !strings.Contains(string(encoded), "release qualification") {
		t.Fatal(string(encoded))
	}
	ordinary, err := f.store.EnqueueRun(t.Context(), model.RunSpec{RepoID: f.repo.ID, RefKind: model.RefBranch, Ref: "feature/held", SHA: strings.Repeat("a", 40), Actor: "developer", Trigger: "branch"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.ClaimNextRun(t.Context()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("ordinary work was claimed: %v", err)
	}
	run, err := f.store.Run(t.Context(), ordinary.ID)
	if err != nil || run.Status != model.RunQueued || run.AdmissionSequence != ordinary.AdmissionSequence {
		t.Fatalf("hold changed queued run: %+v %v", run, err)
	}
	shown, reason := control.observeScheduling(t.Context(), run, nil, false)
	if shown.State != "queued" || !strings.Contains(shown.Message, "queue held by admin: release qualification, until ") || !strings.Contains(reason, "queue held by admin: release qualification, until ") {
		t.Fatalf("hold reason=%q observation=%+v", reason, shown)
	}
	value, err = control.Run(t.Context(), actor, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	detail := value.(RunDetailResponse)
	if detail.Run.DisplayStatus != model.DisplayQueued || detail.Run.DisplayReason != displayFailureText(reason) {
		t.Fatal("hold missing from detail reason", detail.Run.DisplayStatus, detail.Run.DisplayReason)
	}
	value, err = control.CallTool(t.Context(), actor, "run_list", json.RawMessage(`{"status":"queued"}`))
	if err != nil {
		t.Fatal(err)
	}
	rows := value.(api.RunListResponse).Runs.([]model.Run)
	if len(rows) != 1 || rows[0].DisplayStatus != model.DisplayQueued || rows[0].DisplayReason != displayFailureText(reason) {
		t.Fatal("hold missing from list reason", rows)
	}

	if _, err = control.CallTool(t.Context(), api.Actor{Identity: "developer"}, "admission_hold", json.RawMessage(`{"on":false,"reason":"not authorized"}`)); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-admin hold: %v", err)
	}
	if _, err = control.CallTool(t.Context(), actor, "admission_hold", json.RawMessage(`{"on":false,"reason":"qualification done"}`)); err != nil {
		t.Fatal(err)
	}
	claimed, err := f.store.ClaimNextRun(t.Context())
	if err != nil || claimed.ID != ordinary.ID || claimed.AdmissionSequence != ordinary.AdmissionSequence {
		t.Fatalf("release lost queue position: %+v %v", claimed, err)
	}
}

func TestAdmissionHoldSkipsPreparedAndUnpreparedClaimsWithoutPreemption(t *testing.T) {
	f := newControlFixture(t)
	gate := f.scheduler.admission
	now := time.Now().UTC()
	gate.now = func() time.Time { return now }
	holder := gate.reserveRun(model.Run{ID: "holder", QueueSequence: 1, RefKind: model.RefBranch})
	gate.mu.Lock()
	holder.prepared = true
	gate.grantLocked()
	gate.mu.Unlock()
	if !granted(gate, holder) {
		t.Fatal("holder not admitted")
	}
	ordinary := gate.reserveRun(model.Run{ID: "ordinary", QueueSequence: 2, RefKind: model.RefBranch, Priority: model.PriorityHighest})
	if _, err := f.scheduler.SetAdmissionHold(t.Context(), "admin", model.AdmissionHoldRequest{On: true, Reason: "release", Duration: time.Hour}); err != nil {
		t.Fatal(err)
	}
	tag := gate.reserveRun(model.Run{ID: "tag", QueueSequence: 3, RefKind: model.RefTag})
	gate.mu.Lock()
	tag.prepared = true
	gate.grantLocked()
	gate.mu.Unlock()
	if !granted(gate, holder) || !granted(gate, tag) || granted(gate, ordinary) {
		t.Fatal("hold preempted a grant or blocked allowed work behind an unsized claim")
	}
	tag.release()
	gate.mu.Lock()
	ordinary.prepared = true
	gate.grantLocked()
	gate.mu.Unlock()
	if granted(gate, ordinary) {
		t.Fatal("prepared ordinary run bypassed hold")
	}
	before := ordinary.order
	now = now.Add(2 * time.Hour)
	gate.mu.Lock()
	gate.grantLocked()
	gate.mu.Unlock()
	if !granted(gate, ordinary) || ordinary.order != before {
		t.Fatal("expiry did not preserve and resume waiter")
	}
	ordinary.release()
	holder.release()
}

func TestAdmissionHoldDispatchAndGroupBypass(t *testing.T) {
	f := newControlFixture(t)
	gate := f.scheduler.admission
	if _, err := f.scheduler.SetAdmissionHold(t.Context(), "admin", model.AdmissionHoldRequest{On: true, Reason: "release"}); err != nil {
		t.Fatal(err)
	}
	var held []*admissionReservation
	for i := int64(1); i <= 6; i++ {
		r := gate.reserveRun(model.Run{ID: "ordinary", QueueSequence: i, RefKind: model.RefBranch})
		gate.mu.Lock()
		r.prepared = true
		r.group = "shared"
		gate.mu.Unlock()
		held = append(held, r)
	}
	if !f.scheduler.dispatchSlotAvailable(len(held)) {
		t.Fatal("held claims consumed dispatch ceiling")
	}
	tag := gate.reserveRun(model.Run{ID: "tag", QueueSequence: 7, RefKind: model.RefTag})
	gate.mu.Lock()
	tag.prepared = true
	tag.group = "shared"
	gate.grantLocked()
	gate.mu.Unlock()
	if !granted(gate, tag) {
		t.Fatal("held group waiter blocked allowed tag")
	}
	for _, r := range held {
		if granted(gate, r) {
			t.Fatal("ordinary waiter escaped")
		}
	}
	if _, err := f.scheduler.SetAdmissionHold(t.Context(), "admin", model.AdmissionHoldRequest{Reason: "done"}); err != nil {
		t.Fatal(err)
	}
	if granted(gate, held[0]) {
		t.Fatal("clearing hold preempted running group holder")
	}
	tag.release()
	for _, r := range held {
		if !granted(gate, r) {
			t.Fatal("cleared hold lost group FIFO", r.sequence)
		}
		r.release()
	}
}

type blockedHoldStore struct {
	*store.Store
	entered chan struct{}
	resume  chan struct{}
	fail    bool
	lostAck bool
}

func (s *blockedHoldStore) SetAdmissionHold(ctx context.Context, actor string, request model.AdmissionHoldRequest) (model.AdmissionHold, error) {
	close(s.entered)
	select {
	case <-s.resume:
	case <-ctx.Done():
		return model.AdmissionHold{}, ctx.Err()
	}
	if s.fail {
		return model.AdmissionHold{}, errors.New("audit unavailable")
	}
	hold, err := s.Store.SetAdmissionHold(ctx, actor, request)
	if err == nil && s.lostAck {
		return model.AdmissionHold{}, errors.New("lost commit acknowledgement")
	}
	return hold, err
}

func TestAdmissionHoldWriteFenceKeepsGateResponsive(t *testing.T) {
	for _, mode := range []string{"success", "rollback", "lost-ack"} {
		t.Run(mode, func(t *testing.T) {
			f := newControlFixture(t)
			gate := f.scheduler.admission
			holder := gate.reserveRun(model.Run{ID: "running", QueueSequence: 1, RefKind: model.RefBranch})
			gate.mu.Lock()
			holder.prepared = true
			gate.grantLocked()
			gate.mu.Unlock()
			state := &blockedHoldStore{Store: f.store, entered: make(chan struct{}), resume: make(chan struct{}), fail: mode == "rollback", lostAck: mode == "lost-ack"}
			f.scheduler.store = state
			done := make(chan error, 1)
			go func() {
				_, err := f.scheduler.SetAdmissionHold(t.Context(), "admin", model.AdmissionHoldRequest{On: true, Reason: "release"})
				done <- err
			}()
			<-state.entered
			responsive := make(chan *admissionReservation, 1)
			go func() {
				holder.release()
				_ = f.scheduler.AdmissionState()
				r := gate.reserveRun(model.Run{ID: "waiting", QueueSequence: 2, RefKind: model.RefBranch})
				gate.mu.Lock()
				r.prepared = true
				gate.grantLocked()
				gate.mu.Unlock()
				responsive <- r
			}()
			var r *admissionReservation
			select {
			case r = <-responsive:
			case <-time.After(testStartTimeout(time.Second)):
				close(state.resume)
				t.Fatal("SQLite write held gate mutex")
			}
			if granted(gate, r) {
				close(state.resume)
				t.Fatal("grant passed pending hold write")
			}
			close(state.resume)
			err := <-done
			if (mode == "success") != (err == nil) {
				t.Fatal(mode, err)
			}
			if granted(gate, r) != (mode == "rollback") {
				t.Fatal("wrong grant after write/readback", mode)
			}
			if f.scheduler.AdmissionHold().On != (mode != "rollback") {
				t.Fatal("cached hold differs from commit", mode)
			}
			r.release()
		})
	}
}

func TestAdmissionHoldRestartLoadsOriginalDeadline(t *testing.T) {
	f := newRecoveryFixture(t)
	original, err := f.store.SetAdmissionHold(t.Context(), "admin", model.AdmissionHoldRequest{On: true, Reason: "restart", Allow: []string{}, Duration: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	scheduler := f.scheduler(t, &recoveryJobs{})
	if got := scheduler.AdmissionState().Hold; got == nil || !got.On || !got.ExpiresAt.Equal(original.ExpiresAt) {
		t.Fatal(got)
	}
	gate := scheduler.admission
	r := gate.reserveRun(model.Run{ID: "ordinary", QueueSequence: 1, RefKind: model.RefBranch})
	gate.mu.Lock()
	r.prepared = true
	gate.grantLocked()
	gate.mu.Unlock()
	if granted(gate, r) {
		t.Fatal("new scheduler ignored persisted hold")
	}
	gate.mu.Lock()
	gate.now = func() time.Time { return original.ExpiresAt }
	gate.grantLocked()
	gate.mu.Unlock()
	if !granted(gate, r) || scheduler.AdmissionState().Hold.On {
		t.Fatal("deadline did not expire after restart")
	}
	r.release()
}

func TestAdmissionHoldExpiryResumesDispatchWithoutOperator(t *testing.T) {
	f := newControlFixture(t, JobResult{Status: model.RunPassed, Phase: "passed", Steps: []model.StepResult{stepResult(model.StepPassed)}})
	hold, err := f.scheduler.SetAdmissionHold(t.Context(), "admin", model.AdmissionHoldRequest{On: true, Reason: "short hold", Allow: []string{}, Duration: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	run, err := f.scheduler.EnqueueCI(t.Context(), CIRequest{EventID: "expiry", Repository: f.repo, Branch: "feature/expiry", SHA: strings.Repeat("d", 40), Actor: "developer"})
	if err != nil {
		t.Fatal(err)
	}
	ticks := make(chan time.Time, 1)
	f.scheduler.dispatchTicks = ticks
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- f.scheduler.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(testStartTimeout(5 * time.Second)):
			t.Error("scheduler did not stop")
		}
	}()
	// Exercise the production idle-poll path at the persisted absolute deadline.
	timer := time.NewTimer(time.Until(hold.ExpiresAt))
	defer timer.Stop()
	<-timer.C
	ticks <- time.Now()
	deadline := time.NewTimer(testStartTimeout(5 * time.Second))
	defer deadline.Stop()
	for {
		changed := f.signals.Run(run.ID)
		current, err := f.store.Run(t.Context(), run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.Status == model.RunPassed {
			break
		}
		if current.Status.Terminal() {
			t.Fatal("unexpected terminal status", current.Status, current.Reason)
		}
		select {
		case <-changed:
		case <-deadline.C:
			t.Fatal("expired hold did not resume dispatch")
		}
	}
	if f.scheduler.AdmissionHold().On {
		t.Fatal("expired hold still shown active")
	}
}

func TestAdmissionHoldWorkflowOnlyKeepsRunningGrantAndSlotCeiling(t *testing.T) {
	f := newControlFixture(t)
	gate := f.scheduler.admission
	gate.maxRuns = 1
	f.scheduler.maxConcurrent = 1
	holder := gate.reserveRun(model.Run{ID: "running", RepoID: 1, QueueSequence: 1, RefKind: model.RefBranch})
	releaseHolder := acquireRequestWithinBudget(t, holder, groupRequest("running", 1, pipeline.XL, ""))
	if _, err := f.scheduler.SetAdmissionHold(t.Context(), "admin", model.AdmissionHoldRequest{On: true, Reason: "release only"}); err != nil {
		t.Fatal(err)
	}
	ordinary := gate.reserveRun(model.Run{ID: "ordinary", RepoID: 1, QueueSequence: 2, RefKind: model.RefBranch})
	ordinaryReady := make(chan func(), 1)
	go acquireRequestForTest(ordinary, groupRequest("ordinary", 1, pipeline.XL, ""), ordinaryReady)
	awaitWaiting(t, ordinary)
	// A running grant still consumes its slot even when another claim is held.
	if f.scheduler.dispatchSlotAvailable(2) {
		t.Fatal("running workflow lost its dispatch slot")
	}
	tag := gate.reserveRun(model.Run{ID: "release-tag", RepoID: 1, QueueSequence: 3, RefKind: model.RefTag})
	tagReady := make(chan func(), 1)
	go acquireRequestForTest(tag, groupRequest("release-tag", 1, pipeline.XL, ""), tagReady)
	awaitWaiting(t, tag)
	state := f.scheduler.AdmissionState()
	if !granted(gate, holder) || granted(gate, tag) || state.Runs != 1 || state.Used != 1 {
		t.Fatal("hold displaced running work or exceeded one workflow", state)
	}
	releaseHolder()
	releaseTag := awaitAcquired(t, tagReady)
	if granted(gate, ordinary) {
		t.Fatal("ordinary XL bypassed hold")
	}
	releaseTag()
	if state = f.scheduler.AdmissionState(); state.Runs != 0 || !state.Hold.On {
		t.Fatal("release changed hold", state)
	}
	if _, err := f.scheduler.SetAdmissionHold(t.Context(), "admin", model.AdmissionHoldRequest{Reason: "qualification finished"}); err != nil {
		t.Fatal(err)
	}
	releaseOrdinary := awaitAcquired(t, ordinaryReady)
	if state = f.scheduler.AdmissionState(); state.Runs != 1 || state.Used != 1 || state.MaxRuns != 1 {
		t.Fatal("workflow slot accounting changed", state)
	}
	releaseOrdinary()
}

func TestAdmissionHoldRuntimeCeilingAppliesUnderHold(t *testing.T) {
	f := newControlFixture(t)
	f.scheduler.capacityFile = filepath.Join(t.TempDir(), "max-concurrent-jobs")
	if _, err := f.scheduler.SetAdmissionHold(t.Context(), "admin", model.AdmissionHoldRequest{On: true, Reason: "qualification"}); err != nil {
		t.Fatal(err)
	}
	writeCapacity(t, f.scheduler, "1")
	f.scheduler.refreshRuntimeCapacity(t.Context())
	state := f.scheduler.AdmissionState()
	if state.Configuration.Error != "" || state.Configuration.MaxConcurrentJobs != 1 || state.Configuration.Source != "file" || state.MaxRuns != 1 || !state.Hold.On {
		t.Fatal("runtime ceiling not applied or hold changed", state)
	}
	tag := f.scheduler.admission.reserveRun(model.Run{ID: "tag", QueueSequence: 1, RefKind: model.RefTag})
	release := acquireRequestWithinBudget(t, tag, admissionRequest{})
	defer release()
	if state = f.scheduler.AdmissionState(); state.Runs != 1 || state.Used != 1 {
		t.Fatal("hold restored numeric admission", state)
	}
}
