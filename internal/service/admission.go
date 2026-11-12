package service

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

// admissionGate tracks submission lifecycle and the remaining temporary holds.
type admissionGate struct {
	adminHold    model.AdmissionHold
	holdUpdating bool
	mu           sync.Mutex
	// now is the gate's clock; nil is time.Now.
	now             func() time.Time
	priorityChanges map[string]bool
	// runs counts reservations holding a grant: runs whose Workflow Oberth
	// submitted or is about to submit.
	runs         int
	waiters      []*admissionReservation
	reservations map[int64]*admissionReservation
	// onPark, when set, is called under the lock (it must not block) after a
	// reservation is prepared while some prepared reservation is parked by
	// the administrator hold, so the dispatch loop can claim past it.
	onPark func()
	// onGrant, when set, is called under the lock (it must not block) after
	// a grant, so the dispatch loop can claim the slot the grant may leave.
	onGrant func()
	// diskFloor is an opt-in disk-space admission hold (#673). When non-nil
	// and below the configured floor, no new runs are admitted until disk
	// frees up.
	diskFloor *diskFloorCheck
}

type admissionReservation struct {
	// requests is the effective peak Pod request reservation.
	gate     *admissionGate
	sequence int64
	// order retains restart FIFO position; sequence stays the unique run key.
	order int64

	priority               model.RunPriority
	priorityReason         string
	priorityOverride       model.RunPriority
	priorityOverrideReason string
	priorityAccepted       model.RunPriority
	priorityAcceptedReason string
	// priorityLookupFailed withholds automatic tag urgency while the
	// repository's release policy could not be read.
	priorityLookupFailed bool
	queuedAt             time.Time
	runID                string
	// repoID, repo and ref identify the run when it blocks the queue head
	// (#1021); preparedAt is when it started waiting for a slot.
	repoID       int64
	repo         string
	ref          string
	preparedAt   time.Time
	releaseRun   bool
	promotionRun bool
	// claimedAt and grantedAt date the reservation's other states for the
	// admission view (#1099).
	claimedAt time.Time
	grantedAt time.Time
	refKind   model.RefKind
	sha       string
	prepared  bool
	granted   bool
	submitted bool
	delayed   bool
	released  bool
	canceled  bool
	// retained marks a grant whose Workflow may still be running while its
	// owner stops on an error that leaves the run owned for the restart
	// pass. It stays counted for the rest of this process.
	retained bool
	// abandoned marks an ungranted reservation withdrawn because its run was
	// terminalized by someone other than its owner (a newer push, a restart
	// requeue or an operator cancel) while it was still preparing or waiting.
	abandoned bool
	ready     chan struct{}
}

// errAdmissionAbandoned tells the owner of a waiting run that its reservation
// was withdrawn because the durable run is no longer active. It is not a
// scheduler failure: the owner settles the run's cancellation and returns.
var errAdmissionAbandoned = errors.New("service: admission abandoned: run is no longer active")

// admissionRequest is what a claimed run knows once its immutable checkout has
// been read: its identity.
type admissionRequest struct {
	RunID                string
	RepoID               int64
	Repo                 string
	Ref                  string
	PriorityLookupFailed bool
	RefKind              model.RefKind
	SHA                  string
}

// newAdmissionGate builds a gate; production configures its CPU pool.
func newAdmissionGate() *admissionGate {
	return &admissionGate{reservations: make(map[int64]*admissionReservation)}
}

func (gate *admissionGate) clock() time.Time {
	if gate.now != nil {
		return gate.now()
	}
	return time.Now()
}

func (gate *admissionGate) reservePriorityRun(sequence, order int64, run model.Run) *admissionReservation {
	if order <= 0 {
		order = sequence
	}
	reservation := &admissionReservation{gate: gate, sequence: sequence, order: order, ready: make(chan struct{}), claimedAt: time.Now().UTC()}
	reservation.runID, reservation.repoID, reservation.ref = run.ID, run.RepoID, run.Ref
	reservation.refKind, reservation.sha = run.RefKind, run.SHA
	reservation.releaseRun, reservation.promotionRun = run.Release, run.Trigger == "promotion"
	reservation.priority, reservation.priorityReason, reservation.queuedAt = run.Priority, run.PriorityReason, run.QueuedAt
	reservation.priorityOverride, reservation.priorityOverrideReason = run.PriorityOverride, run.PriorityOverrideReason
	reservation.priorityAccepted, reservation.priorityAcceptedReason = run.PriorityAccepted, run.PriorityAcceptedReason
	gate.mu.Lock()
	gate.reservations[sequence] = reservation
	gate.waiters = append(gate.waiters, reservation)
	gate.sortWaitersLocked()
	gate.grantLocked()
	gate.mu.Unlock()
	return reservation
}

// reserveRun is reserve for a claimed run: the reservation carries the run's
// identity from the claim on, so a run still preparing (checking out, reading
// its document) is already addressable by its run ID.
func (gate *admissionGate) reserveRun(run model.Run) *admissionReservation {
	return gate.reservePriorityRun(run.QueueSequence, run.AdmissionSequence, run)
}

func admissionBefore(a, b *admissionReservation) bool {
	return admissionBeforeAt(a, b, a.gate.clock())
}

func admissionBeforeAt(a, b *admissionReservation, now time.Time) bool {
	ap, bp := a.effectivePriority(now), b.effectivePriority(now)
	if ap != bp {
		return ap.Rank() > bp.Rank()
	}
	// Within an effective class the order is FIFO: an aged normal run joins
	// high at its own position, so every high run accepted after it waits
	// behind it. That bounds its wait without lifting it above highest.
	if a.order != b.order {
		return a.order < b.order
	}
	return a.sequence < b.sequence
}

// acquireRequest completes the reservation and waits until it is granted.
func (reservation *admissionReservation) acquireRequest(ctx context.Context, request admissionRequest) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	gate := reservation.gate
	gate.mu.Lock()
	if reservation.canceled || reservation.released {
		abandoned := reservation.abandoned
		gate.mu.Unlock()
		if abandoned {
			return nil, errAdmissionAbandoned
		}
		return nil, context.Canceled
	}
	if request.RunID != "" {
		reservation.runID = request.RunID
	}
	reservation.repoID, reservation.repo, reservation.ref = request.RepoID, request.Repo, request.Ref
	if request.RefKind != "" {
		reservation.refKind, reservation.sha = request.RefKind, request.SHA
	}
	if !reservation.prepared {
		reservation.preparedAt = time.Now().UTC()
	}
	reservation.priorityLookupFailed = request.PriorityLookupFailed
	reservation.prepared = true
	gate.grantLocked()
	granted := reservation.granted
	reservation.delayed = !granted
	// Preparing may leave this run parked by the administrator hold.
	if gate.onPark != nil && gate.parkedLocked() > 0 {
		gate.onPark()
	}
	gate.mu.Unlock()
	if granted {
		return reservation.release, nil
	}

	select {
	case <-reservation.ready:
		gate.mu.Lock()
		abandoned := reservation.abandoned
		gate.mu.Unlock()
		if abandoned {
			return nil, errAdmissionAbandoned
		}
		return reservation.release, nil
	case <-ctx.Done():
		gate.mu.Lock()
		if reservation.granted {
			gate.releaseLocked(reservation)
		} else if !reservation.canceled {
			reservation.canceled = true
			gate.removeLocked(reservation)
			gate.grantLocked()
		}
		gate.mu.Unlock()
		return nil, ctx.Err()
	}
}

func (reservation *admissionReservation) wasDelayed() bool {
	gate := reservation.gate
	gate.mu.Lock()
	defer gate.mu.Unlock()
	return reservation.delayed
}

func (reservation *admissionReservation) cancel() {
	gate := reservation.gate
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if reservation.released || reservation.canceled || reservation.retained {
		return
	}
	if reservation.granted {
		gate.releaseLocked(reservation)
		return
	}
	reservation.canceled = true
	gate.removeLocked(reservation)
	gate.grantLocked()
}

// abandon withdraws the reservation of a run that was terminalized while it
// had not been granted: still preparing or waiting. Such a run owns no
// Workflow, so nothing has to be proved gone first. Its place at the head of
// the FIFO ends now rather than when its owner happens to wake, so the same
// pass admits the runs it was blocking. A granted reservation is never abandoned here: its owner releases
// it only once the Workflow is terminal or its deletion was accepted.
func (gate *admissionGate) abandon(sequence int64) bool {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	reservation := gate.reservations[sequence]
	if reservation == nil || reservation.granted || reservation.released || reservation.canceled || reservation.retained {
		return false
	}
	reservation.canceled = true
	reservation.abandoned = true
	gate.removeLocked(reservation)
	close(reservation.ready)
	gate.grantLocked()
	return true
}

func (reservation *admissionReservation) release() {
	gate := reservation.gate
	gate.mu.Lock()
	defer gate.mu.Unlock()
	gate.releaseLocked(reservation)
}

// releaseAfterCleanup clears a retained grant only after its owner has durable
// proof that the external resources are gone. Clear and release under one lock.
func (reservation *admissionReservation) releaseAfterCleanup() {
	gate := reservation.gate
	gate.mu.Lock()
	defer gate.mu.Unlock()
	reservation.retained = false
	gate.releaseLocked(reservation)
}

// retain keeps a granted reservation counted while a Workflow may still be
// running. Exact-run cleanup or startup reconciliation must confirm absence
// before release.
func (reservation *admissionReservation) retain() {
	gate := reservation.gate
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if reservation.granted && !reservation.released {
		reservation.retained = true
	}
}

// grantLocked applies the remaining explicit holds without bounding preparation.
func (gate *admissionGate) grantLocked() {
	if gate.holdUpdating {
		return
	}
	now := gate.clock()
	gate.sortWaitersLocked()
	for index := 0; index < len(gate.waiters); {
		reservation := gate.waiters[index]
		if gate.adminHeldLocked(reservation) {
			index++
			continue
		}
		if !reservation.prepared || gate.priorityChanges[reservation.runID] {
			index++
			continue
		}
		if !gate.canGrantLocked(reservation) {
			if gate.diskFloor != nil && !gate.diskFloor.freeAboveFloor() {
				return
			}
			index++
			continue
		}
		gate.waiters = append(gate.waiters[:index], gate.waiters[index+1:]...)
		reservation.granted = true
		reservation.grantedAt = now.UTC()
		gate.runs++
		gate.sortWaitersLocked()
		index = 0
		close(reservation.ready)
		if gate.onGrant != nil {
			gate.onGrant()
		}

	}
}

// canGrantLocked checks non-pressure admission constraints.
func (gate *admissionGate) canGrantLocked(reservation *admissionReservation) bool {
	if gate.adminHeldLocked(reservation) {
		return false
	}
	// Disk floor hold (#673): when enabled and the filesystem is below the
	// configured floor, hold all new admissions until disk frees up. The
	// check uses a cached statfs result (sub-microsecond syscall, 5 s TTL)
	// so calling it under the lock is safe.
	return (gate.diskFloor == nil || gate.diskFloor.freeAboveFloor())
}

func (gate *admissionGate) releaseLocked(reservation *admissionReservation) {
	if !reservation.granted || reservation.released || reservation.retained {
		return
	}
	reservation.released = true
	delete(gate.reservations, reservation.sequence)
	gate.runs--
	gate.grantLocked()
}

func (gate *admissionGate) removeLocked(reservation *admissionReservation) {
	delete(gate.reservations, reservation.sequence)
	for index, candidate := range gate.waiters {
		if candidate != reservation {
			continue
		}
		gate.waiters = append(gate.waiters[:index], gate.waiters[index+1:]...)
		return
	}
}

// parkedLocked counts the waiters the administrator hold parks.
func (gate *admissionGate) parkedLocked() int {
	parked := 0
	for _, reservation := range gate.waiters {
		if gate.adminHeldLocked(reservation) {
			parked++
		}
	}
	return parked
}

// headBlockerLocked returns the waiter that stops the grant pass: the first
// claimed run in queue order that is prepared, not parked by the hold, and
// still not granted. grantLocked has already admitted every waiter it could,
// so that run is waiting on the disk floor or host pressure, and every
// unparked run claimed after it waits behind it. Nil while the head is still
// preparing or nothing waits.
func (gate *admissionGate) headBlockerLocked() *admissionReservation {
	for _, reservation := range gate.waiters {
		if gate.adminHeldLocked(reservation) {
			continue
		}
		if !reservation.prepared {
			return nil
		}
		return reservation
	}
	return nil
}

func (gate *admissionGate) blockerLocked(reservation *admissionReservation) *model.AdmissionBlocker {
	blocker := &model.AdmissionBlocker{
		RunID: reservation.runID, RepoID: reservation.repoID, Repo: reservation.repo, Ref: reservation.ref,
		Used:         gate.runs,
		WaitingSince: reservation.preparedAt,
	}
	if gate.diskFloor != nil && !gate.diskFloor.freeAboveFloor() {
		blocker.DiskFloor = true
	}
	return blocker
}

// AdmissionBlocker reports the run at the head of the admission queue that
// holds back every run claimed after it, or nil (#1021).
func (scheduler *Scheduler) AdmissionBlocker() *model.AdmissionBlocker {
	gate := scheduler.admission
	gate.mu.Lock()
	defer gate.mu.Unlock()
	head := gate.headBlockerLocked()
	if head == nil {
		return nil
	}
	return gate.blockerLocked(head)
}

// AdmissionSnapshot reports only this run's reservation.
// No reservation means unknown, never proof of admission or a failed scheduler.
func (scheduler *Scheduler) AdmissionSnapshot(sequence int64) *model.AdmissionObservation {
	gate := scheduler.admission
	gate.mu.Lock()
	defer gate.mu.Unlock()
	reservation := gate.reservations[sequence]
	if reservation == nil {
		return nil
	}
	result := &model.AdmissionObservation{State: "preparing", Used: gate.runs, Runs: gate.runs}
	if reservation.prepared {
		result.State = "waiting"
	}
	if reservation.granted {
		result.State = "admitted"
		return result
	}
	for i, candidate := range gate.waiters {
		if candidate == reservation {
			result.Position = i + 1
			break
		}
	}
	if reservation.prepared {
		if head := gate.headBlockerLocked(); head == reservation {
			result.HeadOfQueue = true
			result.BlockedBy = gate.blockerLocked(head)
		} else if head != nil && admissionBefore(head, reservation) {
			result.BlockedBy = gate.blockerLocked(head)
		}
	}
	if gate.diskFloor != nil {
		result.DiskFloor = gate.diskFloor.observation()
	}
	return result
}

// Re-sort on every admission pass because aging changes effective class.
func (gate *admissionGate) sortWaitersLocked() {
	now := gate.clock()
	sort.SliceStable(gate.waiters, func(i, j int) bool {
		a, b := gate.waiters[i], gate.waiters[j]
		return admissionBeforeAt(a, b, now)
	})
}
