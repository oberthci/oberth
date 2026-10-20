package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

// ErrAdmissionReleaseRefused reports a reservation an operator may not
// release: one whose owner is alive and will release it itself.
var ErrAdmissionReleaseRefused = fmt.Errorf("%w: admission release refused", store.ErrInvalidState)

func (gate *weightedAdmission) reservationStateLocked(reservation *admissionReservation) string {
	switch {
	case reservation.granted && reservation.retained:
		return "retained"
	case reservation.granted:
		return "granted"
	case reservation.vmParked:
		return "vm"
	case !reservation.prepared:
		return "preparing"
	}
	if holder, ahead := gate.groupBlockerLocked(reservation); holder != "" || ahead != "" {
		return "group"
	}
	return "waiting"
}

func (gate *weightedAdmission) reservationViewLocked(reservation *admissionReservation) model.AdmissionReservation {
	view := model.AdmissionReservation{
		RunID: reservation.runID, RepoID: reservation.repoID, Repo: reservation.repo, Ref: reservation.ref,
		Size: string(reservation.size), Weight: reservation.weight, Group: reservation.groupName,
		State: gate.reservationStateLocked(reservation), Backfilled: reservation.granted && reservation.backfilled,
	}
	if reservation.repo != "" && reservation.runID != "" {
		if name, err := deterministicJobName(reservation.repo, reservation.runID); err == nil {
			view.Workflow = name
		}
	}
	switch view.State {
	case "granted", "retained":
		view.Since = reservation.grantedAt
	case "preparing":
		view.Since = reservation.claimedAt
	default:
		view.Since = reservation.preparedAt
		for index, candidate := range gate.waiters {
			if candidate == reservation {
				view.Position = index + 1
				break
			}
		}
	}
	return view
}

// AdmissionState reports every reservation the gate holds, granted, retained
// or waiting, and every concurrency group in use (#1099).
func (scheduler *Scheduler) AdmissionState() *model.AdmissionState {
	gate := scheduler.admission
	gate.mu.Lock()
	defer gate.mu.Unlock()
	state := &model.AdmissionState{
		Capacity: gate.capacity, Used: gate.used, Runs: gate.runs, MaxRuns: gate.maxRuns,
		MaxHeadWaitSeconds: int64(gate.maxHeadWait / time.Second),
		Reservations:       []model.AdmissionReservation{}, Groups: []model.AdmissionGroup{},
	}
	sequences := make([]int64, 0, len(gate.reservations))
	for sequence := range gate.reservations {
		sequences = append(sequences, sequence)
	}
	sort.Slice(sequences, func(i, j int) bool { return sequences[i] < sequences[j] })
	groups := map[string]*model.AdmissionGroup{}
	var groupOrder []string
	for _, sequence := range sequences {
		reservation := gate.reservations[sequence]
		view := gate.reservationViewLocked(reservation)
		state.Reservations = append(state.Reservations, view)
		if reservation.group == "" {
			continue
		}
		group := groups[reservation.group]
		if group == nil {
			group = &model.AdmissionGroup{Name: reservation.groupName, RepoID: reservation.repoID, Repo: reservation.repo}
			groups[reservation.group] = group
			groupOrder = append(groupOrder, reservation.group)
		}
		if holder := gate.groups[reservation.group]; holder == reservation {
			group.Holder = reservation.runID
		} else if !reservation.granted {
			group.Waiting = append(group.Waiting, reservation.runID)
		}
	}
	for _, key := range groupOrder {
		state.Groups = append(state.Groups, *groups[key])
	}
	if head := gate.headBlockerLocked(); head != nil {
		state.Blocker = gate.blockerLocked(head)
	}
	return state
}

// ReleaseAdmission is the operator's way to clear a reservation no owner
// will release (#1099): a granted reservation still held for a run that is
// already terminal (a hung or lost owner), an ungranted one of a terminal
// run, or, while the stopping scheduler drains, a retained one. A reservation whose run is still
// active and owned is refused; run_cancel ends such a run. Before releasing a
// granted reservation the run's Workflow is deleted through the engine (an
// accepted deletion is the same proof the scheduler itself releases on);
// force releases even when that fails, and the audit records it unverified.
func (scheduler *Scheduler) ReleaseAdmission(ctx context.Context, runID, actor string, force bool) (model.AdmissionRelease, error) {
	runID = strings.TrimSpace(runID)
	if runID == "" || strings.TrimSpace(actor) == "" {
		return model.AdmissionRelease{}, fmt.Errorf("%w: run ID and actor are required", ErrInvalidInput)
	}
	gate := scheduler.admission
	gate.mu.Lock()
	var reservation *admissionReservation
	for _, candidate := range gate.reservations {
		if candidate.runID == runID {
			reservation = candidate
			break
		}
	}
	var view model.AdmissionReservation
	if reservation != nil {
		view = gate.reservationViewLocked(reservation)
	}
	gate.mu.Unlock()
	if reservation == nil {
		return model.AdmissionRelease{}, fmt.Errorf("%w: no admission reservation is held for run %s", store.ErrNotFound, runID)
	}
	run, err := scheduler.store.Run(ctx, runID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return model.AdmissionRelease{}, fmt.Errorf("load run %s: %w", runID, err)
	}
	if errors.Is(err, store.ErrNotFound) && !force {
		return model.AdmissionRelease{}, fmt.Errorf("%w: run %s has no durable record; release with force only if no owner holds it",
			ErrAdmissionReleaseRefused, runID)
	}
	active := err == nil && run.Status.Active()
	result := model.AdmissionRelease{RunID: runID, State: view.State, Weight: view.Weight, Group: view.Group, Workflow: run.JobName, Forced: force}
	switch view.State {
	case "retained":
	case "granted":
		if active {
			return model.AdmissionRelease{}, fmt.Errorf("%w: run %s is %s and its owner releases its admission itself; cancel the run with run_cancel instead",
				ErrAdmissionReleaseRefused, runID, run.Status)
		}
	default:
		if active {
			return model.AdmissionRelease{}, fmt.Errorf("%w: run %s is %s and still %s for admission; cancel the run with run_cancel instead",
				ErrAdmissionReleaseRefused, runID, run.Status, view.State)
		}
	}
	result.Verified = true
	if view.State == "granted" || view.State == "retained" {
		switch run.JobName {
		case "":
			// A granted run with no recorded Workflow name is either between
			// its grant and the name being recorded, or in a trusted KVM
			// preflight whose own Workflow is named elsewhere. Nothing here
			// can prove that Workflow gone.
			if !force {
				return model.AdmissionRelease{}, fmt.Errorf("%w: run %s holds a grant but has no recorded Workflow (a trusted preflight may still be running); "+
					"its Workflow cannot be proven gone, so release it only with force", ErrAdmissionReleaseRefused, runID)
			}
			result.Verified = false
			result.Warning = "released without proof: run " + runID + " has no recorded Workflow (a trusted preflight may still be running)"
		default:
			if deleteErr := scheduler.jobs.Delete(ctx, run.JobName, runID); deleteErr != nil {
				if !force {
					return model.AdmissionRelease{}, fmt.Errorf("%w: could not delete Workflow %s to prove it gone (%w); retry, or release with force",
						ErrAdmissionReleaseRefused, run.JobName, deleteErr)
				}
				result.Verified = false
				result.Warning = "released without proof that Workflow " + run.JobName + " is gone: " + deleteErr.Error()
			}
		}
	}
	// The audit records the outcome, so it is appended under the gate lock
	// once the reservation is known to be still held, and before anything is
	// freed: a failed append changes nothing, and an owner that released the
	// reservation meanwhile leaves no false release in the chain.
	gate.mu.Lock()
	if reservation.released || (reservation.canceled && !reservation.granted) {
		gate.mu.Unlock()
		return model.AdmissionRelease{}, fmt.Errorf("%w: the reservation of run %s was released by its owner meanwhile", store.ErrNotFound, runID)
	}
	details, err := json.Marshal(map[string]any{
		"run_id": runID, "state": view.State, "weight": view.Weight, "group": view.Group,
		"workflow": run.JobName, "verified": result.Verified, "force": force,
	})
	if err == nil {
		_, err = scheduler.auditor.AppendAuditAction(ctx, model.AuditActionSpec{
			Actor: actor, Action: "admission.release", ResourceType: "run", ResourceID: runID, Details: string(details),
		})
	}
	if err != nil {
		gate.mu.Unlock()
		return model.AdmissionRelease{}, fmt.Errorf("audit admission release: %w", err)
	}
	if reservation.granted {
		reservation.retained = false
		gate.releaseLocked(reservation)
	} else {
		reservation.canceled = true
		reservation.abandoned = true
		gate.removeLocked(reservation)
		close(reservation.ready)
		gate.grantLocked()
	}
	gate.mu.Unlock()
	scheduler.signalQueue()
	return result, nil
}

// AdmissionReleaser is the scheduler capability behind admission_release.
type AdmissionReleaser interface {
	ReleaseAdmission(ctx context.Context, runID, actor string, force bool) (model.AdmissionRelease, error)
}

func (service *API) releaseAdmission(ctx context.Context, actor api.Actor, runID string, force bool) (model.AdmissionRelease, error) {
	if !actor.Admin {
		return model.AdmissionRelease{}, fmt.Errorf("%w: admission_release requires an admin uplink", ErrForbidden)
	}
	releaser, ok := service.admission.(AdmissionReleaser)
	if !ok {
		return model.AdmissionRelease{}, fmt.Errorf("%w: admission release", ErrUnavailable)
	}
	return releaser.ReleaseAdmission(ctx, runID, actor.Identity, force)
}
