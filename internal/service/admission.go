package service

import (
	"context"
	"errors"
	"fmt"
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
// workflow controller. The gate keeps only host safety that is independent
// of workflow contents: the optional run ceiling, the administrator hold,
// the disk floor and the PSI pressure valve.
type admissionGate struct {
	adminHold      model.AdmissionHold
	holdUpdating   bool
	mu             sync.Mutex
	capacityConfig model.AdmissionCapacityConfig
	// now is the gate's clock; nil is time.Now.
	now             func() time.Time
	priorityChanges map[string]bool
	// maxRuns, when positive, caps how many runs hold a grant at once. Zero
	// is no ceiling: every prepared run is admitted unless the hold, the disk
	// floor or host pressure says otherwise.
	maxRuns      int
	runs         int
	waiters      []*admissionReservation
	reservations map[int64]*admissionReservation
	// groups maps a shared concurrency group name to its granted reservations.
	groups map[string][]*admissionReservation
	// onPark, when set, is called under the lock (it must not block) after a
	// reservation is prepared while some prepared reservation is parked on its
	// group, so the dispatch loop can claim past the parked run.
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
	// group is the shared key of the run's concurrency group and
	// groupName the name the repository declared; both empty when ungrouped.
	group      string
	groupName  string
	groupLimit int
	prepared   bool
	granted    bool
	submitted  bool
	delayed    bool
	released   bool
	canceled   bool
	// retained marks a grant whose Workflow may still be running while its
	// owner stops on an error that leaves the run owned for the restart
	// pass. Its slot and group stay held for the rest of this process
	// instead of passing to the next run.
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

// A live or retained grant can outlast its durable YAML definition. Conflicting
// new work fails that run without taking down the scheduler or releasing holders.
type concurrencyGroupConflict struct {
	group                   string
	limit, conflictingLimit int
}

func (conflict *concurrencyGroupConflict) Error() string {
	return fmt.Sprintf("service: concurrency group %q limit %d conflicts with limit %d", conflict.group, conflict.limit, conflict.conflictingLimit)
}

// admissionRequest is what a claimed run knows once its immutable checkout has
// been read: its identity and, optionally, its shared group and limit.
type admissionRequest struct {
	Requests             *argoworkflow.PodRequests
	RunID                string
	RepoID               int64
	Repo                 string
	Ref                  string
	PriorityLookupFailed bool
	Group                string
	GroupLimit           int
	RefKind              model.RefKind
	SHA                  string
}

// newAdmissionGate builds a gate that admits at most maxRuns runs at once;
// zero or less is no ceiling.
func newAdmissionGate(maxRuns int) *admissionGate {
	return &admissionGate{
		maxRuns:      max(0, maxRuns),
		reservations: make(map[int64]*admissionReservation),
		groups:       make(map[string][]*admissionReservation),
	}
}

func (gate *admissionGate) clock() time.Time {
	if gate.now != nil {
		return gate.now()
	}
	return time.Now()
}

// concurrencyGroupKey is shared by all repositories declaring the same name.
func concurrencyGroupKey(_ int64, group string) string { return group }

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

// acquireRequest completes the reservation and waits until it is granted. A
// grouped run additionally waits until fewer than its declared limit hold the
// same concurrency group; while it does, it holds neither a slot nor its
// place at the head of the queue, so it never delays the claims behind it.
func (reservation *admissionReservation) acquireRequest(ctx context.Context, request admissionRequest) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if request.Group != "" && !argoworkflow.ValidConcurrencyGroup(request.Group) {
		reservation.cancel()
		return nil, fmt.Errorf("service: invalid concurrency group %q", request.Group)
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
	if request.Group != "" {
		reservation.group = concurrencyGroupKey(request.RepoID, request.Group)
		reservation.groupName = request.Group
		reservation.groupLimit = max(1, request.GroupLimit)
		for _, other := range gate.reservations {
			if other != reservation && other.prepared && other.group == reservation.group && max(1, other.groupLimit) != reservation.groupLimit {
				limit, conflictingLimit := reservation.groupLimit, max(1, other.groupLimit)
				gate.mu.Unlock()
				reservation.cancel()
				return nil, &concurrencyGroupConflict{group: request.Group, limit: limit, conflictingLimit: conflictingLimit}
			}
		}
	}
	reservation.prepared = true
	gate.grantLocked()
	granted := reservation.granted
	reservation.delayed = !granted
	// Preparing may park this run on its group, or a
	// later run of the same group that was waiting without anyone ahead of it.
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
// had not been granted: still preparing, waiting for a slot, parked on its
// concurrency group. Such a run owns no Workflow,
// so nothing has to be proved gone first. Its place at the head of the FIFO,
// and its position ahead of later members of its group, end now rather than
// when its owner happens to wake, so the same pass admits the runs it was
// blocking. A granted reservation is never abandoned here: its owner releases
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

// retain keeps a granted reservation's slot and concurrency group held
// while a Workflow may still be running. Passing the group on before observed
// cleanup would let its next member start beside a live Workflow. Exact-run
// cleanup or startup reconciliation must confirm absence before release.
func (reservation *admissionReservation) retain() {
	gate := reservation.gate
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if reservation.granted && !reservation.released {
		reservation.retained = true
	}
}

// grantLocked admits waiters in claim order. An unprepared claim at the head
// holds its place, and so does a prepared head without a slot: slots are
// shared by every run, so the first run that cannot start stops the pass and
// nothing later overtakes it.
//
// A run parked on its concurrency group is the one exception. It is not
// waiting for a slot but for another run of its own group, so it is
// passed over rather than allowed to stop the pass: unrelated claims behind it
// are admitted as usual. Every later run of the same group
// is passed over too, which keeps the group itself in claim order, and the
// parked run regains its place in that order the moment its group is free.
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
		if gate.sharedBlockedLocked(reservation) {
			index++
			continue
		}
		if !gate.canGrantLocked(reservation) {
			return
		}
		// Stop at the first otherwise eligible candidate. A pressure timeout
		// never bypasses priority, group, disk or slot rules.
		if gate.deferPressureLocked(reservation, now) {
			return
		}
		gate.waiters = append(gate.waiters[:index], gate.waiters[index+1:]...)
		reservation.granted = true
		reservation.grantedAt = now.UTC()
		gate.runs++
		if reservation.group != "" {
			gate.groups[reservation.group] = append(gate.groups[reservation.group], reservation)
		}
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

// spareSlotForClaim reports whether the dispatch loop may claim one more run
// past its ceiling: every claimed, unparked run already holds its grant, and
// the run ceiling and the disk floor both still have room. unparked is the
// dispatch loop's count of claimed runs not parked on a group.
func (gate *admissionGate) spareSlotForClaim(unparked int) bool {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	// Every unparked claimed run must already hold its grant: one still
	// checking out or waiting for a slot may need what is spare.
	if len(gate.waiters)-gate.parkedLocked() > 0 || unparked > gate.runs {
		return false
	}
	if gate.maxRuns > 0 && gate.runs >= gate.maxRuns {
		return false
	}
	if gate.diskFloor != nil && !gate.diskFloor.freeAboveFloor() {
		return false
	}
	return true
}

// canGrantLocked admits on the run ceiling alone, after the administrator
// hold and the disk floor.
func (gate *admissionGate) canGrantLocked(reservation *admissionReservation) bool {
	if gate.adminHeldLocked(reservation) {
		return false
	}
	// Disk floor hold (#673): when enabled and the filesystem is below the
	// configured floor, hold all new admissions until disk frees up. The
	// check uses a cached statfs result (sub-microsecond syscall, 5 s TTL)
	// so calling it under the lock is safe.
	if gate.diskFloor != nil && !gate.diskFloor.freeAboveFloor() {
		return false
	}
	return gate.maxRuns <= 0 || gate.runs < gate.maxRuns
}

func (gate *admissionGate) releaseLocked(reservation *admissionReservation) {
	if !reservation.granted || reservation.released || reservation.retained {
		return
	}
	reservation.released = true
	delete(gate.reservations, reservation.sequence)
	gate.runs--
	if reservation.group != "" {
		gate.groups[reservation.group] = deleteReservation(gate.groups[reservation.group], reservation)
		if len(gate.groups[reservation.group]) == 0 {
			delete(gate.groups, reservation.group)
		}
	}
	gate.grantLocked()
}

func deleteReservation(list []*admissionReservation, target *admissionReservation) []*admissionReservation {
	kept := list[:0]
	for _, candidate := range list {
		if candidate != target {
			kept = append(kept, candidate)
		}
	}
	return kept
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

// parkedLocked counts admin-held waiters, and prepared waiters held back by their concurrency
// group: the group is full, or an earlier run of the
// same group is waiting ahead of them.
func (gate *admissionGate) parkedLocked() int {
	parked := 0
	for _, reservation := range gate.waiters {
		if gate.adminHeldLocked(reservation) {
			parked++
			continue
		}
		if reservation.prepared && gate.sharedBlockedLocked(reservation) {
			parked++
		}
	}
	return parked
}

// groupBlockerLocked names what a waiting grouped reservation waits for: the
// run holding its group, or else the earlier run of the same group queued
// ahead of it. Both are empty when the reservation is not parked.
func (gate *admissionGate) groupBlockerLocked(reservation *admissionReservation) (holder, ahead string) {
	if reservation.group == "" || reservation.granted {
		return "", ""
	}
	if current := gate.groups[reservation.group]; len(current) >= max(1, reservation.groupLimit) {
		return current[0].runID, ""
	}
	for _, candidate := range gate.waiters {
		if candidate == reservation {
			break
		}
		if candidate.prepared && !gate.adminHeldLocked(candidate) && candidate.group == reservation.group {
			return "", candidate.runID
		}
	}
	return "", ""
}

// sharedBlockedLocked reports a waiter parked on its concurrency group. Such
// a run waits for other runs, not for a slot, so it never stops the grant
// pass for unrelated claims behind it.
func (gate *admissionGate) sharedBlockedLocked(reservation *admissionReservation) bool {
	holder, ahead := gate.groupBlockerLocked(reservation)
	return holder != "" || ahead != ""
}

// headBlockerLocked returns the waiter that stops the grant pass: the first
// claimed run in queue order that is prepared, not parked on its group,
// and still not granted. grantLocked has already admitted every
// waiter it could, so that run is waiting on the run ceiling or the
// disk floor, and every unparked run claimed after it waits behind it. Nil
// while the head is still preparing or nothing waits.
func (gate *admissionGate) headBlockerLocked() *admissionReservation {
	for _, reservation := range gate.waiters {
		if gate.adminHeldLocked(reservation) {
			continue
		}
		if !reservation.prepared {
			return nil
		}
		if gate.sharedBlockedLocked(reservation) {
			continue
		}
		return reservation
	}
	return nil
}

func (gate *admissionGate) blockerLocked(reservation *admissionReservation) *model.AdmissionBlocker {
	blocker := &model.AdmissionBlocker{
		RunID: reservation.runID, RepoID: reservation.repoID, Repo: reservation.repo, Ref: reservation.ref,
		Used: gate.runs, Capacity: gate.maxRuns,
		WaitingSince: reservation.preparedAt,
		Pressure:     gate.pressureWaitLocked(reservation),
	}
	switch {
	case gate.diskFloor != nil && !gate.diskFloor.freeAboveFloor():
		blocker.DiskFloor = true
	case gate.maxRuns > 0 && gate.runs >= gate.maxRuns:
		blocker.RunCeiling = true
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

// AdmissionSnapshot reports only this run's reservation and the run ceiling.
// No reservation means unknown, never proof of admission or a failed scheduler.
func (scheduler *Scheduler) AdmissionSnapshot(sequence int64) *model.AdmissionObservation {
	gate := scheduler.admission
	gate.mu.Lock()
	defer gate.mu.Unlock()
	reservation := gate.reservations[sequence]
	if reservation == nil {
		return nil
	}
	result := &model.AdmissionObservation{Requests: reservation.requests.Clone(), State: "preparing", Used: gate.runs, Capacity: gate.maxRuns, Runs: gate.runs, MaxRuns: gate.maxRuns}
	if reservation.prepared {
		result.State = "waiting"
		result.Group = reservation.groupName
		if result.Group != "" {
			result.GroupLimit = max(1, reservation.groupLimit)
		}
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
	result.GroupHolder, result.GroupAhead = gate.groupBlockerLocked(reservation)
	result.GroupBlocked = result.GroupHolder != "" || result.GroupAhead != ""
	result.GroupRunRepos = map[string]int64{}
	for _, holder := range gate.groups[reservation.group] {
		result.GroupHolders = append(result.GroupHolders, holder.runID)
		result.GroupRunRepos[holder.runID] = holder.repoID
	}
	for _, waiter := range gate.waiters {
		if waiter.runID == result.GroupAhead {
			result.GroupRunRepos[waiter.runID] = waiter.repoID
		}
	}
	if reservation.prepared && !result.GroupBlocked {
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
// claimed: the run ceiling or host pressure, both server-wide facts.
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
	if gate.maxRuns > 0 && gate.runs >= gate.maxRuns {
		return queuedAdmissionMessage{Text: fmt.Sprintf("%d of %d runs running", gate.runs, gate.maxRuns)}
	}
	return queuedAdmissionMessage{}
}

// Re-sort on every admission pass because aging changes effective class.
func (gate *admissionGate) sortWaitersLocked() {
	now := gate.clock()
	sort.SliceStable(gate.waiters, func(i, j int) bool { return admissionBeforeAt(gate.waiters[i], gate.waiters[j], now) })
}
