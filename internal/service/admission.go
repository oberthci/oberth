package service

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

// admissionGate counts runs. A run claimed from the queue holds a reservation
// here from its claim until its Workflow is terminal; a grant is one run slot.
// Nothing in the gate reads the run's workflow document: what a step needs
// is the step's own resources block, placed by Kubernetes, and what a step
// may share with other steps is native Argo synchronization, queued by the
// workflow controller. How many Workflows run at once is the Argo
// controller's parallelism (docs/where-limits-live.md): Oberth submits every
// granted run and Argo postpones the excess. The gate keeps only host safety
// that is independent of workflow contents: the administrator hold, the disk
// floor and the PSI pressure valve.
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
	pressure  *pressureCheck
}

type admissionReservation struct {
	// requests is the run's peak pod requests, carried for display only.
	requests *argoworkflow.PodRequests
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
	repoID            int64
	repo              string
	ref               string
	preparedAt        time.Time
	pressureRelease   bool
	pressurePromotion bool
	pressureSince     time.Time
	pressureDeferred  bool
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
	Requests             *argoworkflow.PodRequests
	RunID                string
	RepoID               int64
	Repo                 string
	Ref                  string
	PriorityLookupFailed bool
	RefKind              model.RefKind
	SHA                  string
}

// newAdmissionGate builds a gate without a run ceiling.
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
	reservation.pressureRelease, reservation.pressurePromotion = run.Release, run.Trigger == "promotion"
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
	reservation.requests = request.Requests.Clone()
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

// grantLocked admits waiters in claim order. An unprepared claim at the head
// holds its place, and so does a prepared head the disk floor or host
// pressure holds back: the first run that cannot start stops the pass and
// nothing later overtakes it. A run the administrator hold parks is passed
// over.
func (gate *admissionGate) grantLocked() {
	if gate.holdUpdating {
		return
	}
	now := gate.clock()
	if gate.pressure != nil {
		gate.pressure.starvationGranted = false
	}
	gate.pressure.refresh(now)
	gate.sortWaitersLocked()
	gate.preparePressureWaitsLocked(now)
	for index := 0; index < len(gate.waiters); {
		reservation := gate.waiters[index]
		if gate.adminHeldLocked(reservation) {
			index++
			continue
		}
		if !reservation.prepared || gate.priorityChanges[reservation.runID] {
			// A claim still preparing holds its place at the head.
			return
		}
		if !gate.canGrantLocked(reservation) {
			return
		}
		// Stop at the first otherwise eligible candidate. A pressure timeout
		// never bypasses priority or the disk floor.
		if gate.deferPressureLocked(reservation, now) {
			return
		}
		gate.waiters = append(gate.waiters[:index], gate.waiters[index+1:]...)
		reservation.granted = true
		reservation.grantedAt = now.UTC()
		gate.runs++
		close(reservation.ready)
		if gate.onGrant != nil {
			gate.onGrant()
		}
		if gate.pressure != nil && gate.pressure.config.Enabled {
			gate.pressure.lastGrant = gate.clock()
			// Refresh the hold even when no waiter remains, so the next pass
			// and cached status both observe the settle window.
			now = gate.clock()
			gate.pressure.refresh(now)
			gate.preparePressureWaitsLocked(now)
		}
	}
}

// spareSlotForClaim reports whether the dispatch loop may claim one more run:
// no claimed run the administrator hold does not park is still waiting in
// the gate (checking out, or held back by the disk floor or pressure). A
// claim reserves at once, so at most one run waits here at a time. A run
// that already released its grant (publishing) does not count, and below
// the disk floor one run is still claimed, so it can say why it waits.
func (gate *admissionGate) spareSlotForClaim() bool {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	return len(gate.waiters)-gate.parkedLocked() == 0
}

// canGrantLocked admits unless the administrator hold or the disk floor
// says otherwise.
func (gate *admissionGate) canGrantLocked(reservation *admissionReservation) bool {
	if gate.adminHeldLocked(reservation) {
		return false
	}
	// Disk floor hold (#673): when enabled and the filesystem is below the
	// configured floor, hold all new admissions until disk frees up. The
	// check uses a cached statfs result (sub-microsecond syscall, 5 s TTL)
	// so calling it under the lock is safe.
	return gate.diskFloor == nil || gate.diskFloor.freeAboveFloor()
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
		Pressure:     gate.pressureWaitLocked(reservation),
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
	result := &model.AdmissionObservation{Requests: reservation.requests.Clone(), State: "preparing", Used: gate.runs, Runs: gate.runs}
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
	result.Pressure = gate.pressureWaitLocked(reservation)
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

type queuedAdmissionMessage struct {
	Text     string
	Pressure bool
}

// QueuedAdmissionMessage is the reason an unclaimed queued run is not being
// claimed: host pressure, a server-wide fact.
func (scheduler *Scheduler) QueuedAdmissionMessage() queuedAdmissionMessage {
	if scheduler == nil || scheduler.admission == nil {
		return queuedAdmissionMessage{}
	}
	gate := scheduler.admission
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if p := gate.pressureStateLocked(); p != nil && p.Holding {
		return queuedAdmissionMessage{Text: queuePressureReason(&model.PressureDeferral{Resource: p.Resource}), Pressure: true}
	}
	return queuedAdmissionMessage{}
}

// Re-sort on every admission pass because aging changes effective class.
func (gate *admissionGate) sortWaitersLocked() {
	now := gate.clock()
	sort.SliceStable(gate.waiters, func(i, j int) bool { return admissionBeforeAt(gate.waiters[i], gate.waiters[j], now) })
}
