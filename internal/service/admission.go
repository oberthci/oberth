package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

type weightedAdmission struct {
	mu             sync.Mutex
	capacity       int
	capacityConfig model.AdmissionCapacityConfig
	// weights is the operator's weight per size (#1021).
	weights SizeWeights
	// maxHeadWait bounds bounded backfill (#1085): while the queue head has
	// waited less than this, later runs that fit are admitted beside it.
	// Zero disables backfill (strict FIFO).
	maxHeadWait time.Duration
	// now is the gate's clock; nil is time.Now.
	now func() time.Time
	// headEvents carries head-wait origins to persist and clear, so a
	// restarted server keeps the head's wait origin. Sends never block.
	headEvents chan headEvent
	used       int
	// maxRuns, when positive, caps how many runs hold a grant at once. The
	// scheduler mirrors its dispatch ceiling here: runs parked on their own
	// concurrency group stop counting against the dispatch loop, so the loop
	// alone no longer bounds how many claimed runs can be admitted together.
	// Zero leaves the run count to the caller, as it always was.
	maxRuns      int
	runs         int
	waiters      []*admissionReservation
	reservations map[int64]*admissionReservation
	// groups maps a repository-scoped concurrency group key to the granted
	// reservation that holds it (#658).
	groups map[string]*admissionReservation
	// onPark, when set, is called under the lock (it must not block) after a
	// reservation is prepared while some prepared reservation is parked on its
	// group, so the dispatch loop can claim past the parked run.
	onPark func()
	// onGrant, when set, is called under the lock (it must not block) after
	// a grant, so the dispatch loop can claim into weight the grant left
	// spare (#1021).
	onGrant func()
	// diskFloor is an opt-in disk-space admission hold (#673). When non-nil
	// and below the configured floor, no new runs are admitted until disk
	// frees up.
	diskFloor *diskFloorCheck
	pressure  *pressureCheck
}

type admissionReservation struct {
	gate     *weightedAdmission
	sequence int64
	// order retains restart FIFO position; sequence stays the unique run key.
	order int64

	priority               model.RunPriority
	priorityReason         string
	priorityOverride       model.RunPriority
	priorityOverrideReason string
	queuedAt               time.Time
	runID                  string
	// repoID, repo and ref identify the run when it blocks the queue head
	// (#1021); preparedAt is when it started waiting for weight.
	repoID           int64
	repo             string
	ref              string
	preparedAt       time.Time
	pressureSince    time.Time
	pressureDeferred bool
	// claimedAt and grantedAt date the reservation's other states for the
	// admission view (#1099).
	claimedAt time.Time
	grantedAt time.Time
	// refKind and sha complete the durable key of the run's head-wait
	// origin (#1085), which survives a restart's requeue under a new run ID.
	refKind model.RefKind
	sha     string
	// headSince is when this run first waited at the queue head (or the
	// durable origin recorded before a restart); zero while it never has.
	headSince time.Time
	// estimate is the run's predicted duration from history, zero if none.
	estimate time.Duration
	// backfilled marks a grant made beside a waiting head.
	backfilled bool
	kubeVirt   KubeVirtResources
	size       periapsis.Size
	// priorityMaximum caps only automatic tag urgency, not release execution.
	priorityMaximum      periapsis.Size
	priorityLookupFailed bool
	weight               int
	// group is the repository-scoped key of the run's concurrency group and
	// groupName the name the repository declared; both empty when ungrouped.
	group     string
	groupName string
	prepared  bool
	granted   bool
	submitted bool
	delayed   bool
	released  bool
	canceled  bool
	// retained marks a grant whose Workflow may still be running while its
	// owner stops on an error that leaves the run owned for the restart
	// pass. Its weight and group stay held for the rest of this process
	// instead of passing to the next run.
	retained bool
	// vmParked owns no weight, group or dispatch slot while durable VM capacity is busy.
	vmParked bool
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
// been read: its declared size and, optionally, its repository-scoped group.
type admissionRequest struct {
	RunID                string
	RepoID               int64
	Repo                 string
	Ref                  string
	Size                 periapsis.Size
	PriorityMaximum      periapsis.Size
	PriorityLookupFailed bool
	Group                string
	KubeVirt             *KubeVirtResources
	// RefKind and SHA key the durable head-wait origin; HeadSince is that
	// origin when one was recorded before a restart; Estimate is the run's
	// predicted duration from history (#1085).
	RefKind   model.RefKind
	SHA       string
	HeadSince time.Time
	Estimate  time.Duration
}

// newWeightedAdmission builds a gate whose capacity is denominated in size
// weight, not in runs. Callers holding a run-count ceiling must convert with
// newWeightedAdmissionForJobs rather than passing the count directly.
func newWeightedAdmission(capacity int) *weightedAdmission {
	if capacity < 1 {
		capacity = 1
	}
	return &weightedAdmission{
		capacity:     capacity,
		weights:      DefaultSizeWeights,
		reservations: make(map[int64]*admissionReservation),
		groups:       make(map[string]*admissionReservation),
	}
}

// newWeightedAdmissionForJobs converts the operator's configured concurrent-run
// ceiling into the weight budget this gate is denominated in.
//
// The two are different units and conflating them silently caps the whole
// server at one run: a budget of maxConcurrentJobs weight admits only
// floor(maxConcurrentJobs / weight) runs, which for the default M size (weight
// 2) is one run at maxConcurrentJobs=3. Scaling by the default size's weight
// makes maxConcurrentJobs runs of an undeclared-size pipeline fit exactly, so
// the configured number is the number an ordinary repository actually gets.
// A pipeline that declares a heavier size then trades concurrency for
// per-run resources proportionally, instead of collapsing the server to one.
func newWeightedAdmissionForJobs(maxConcurrentJobs int) *weightedAdmission {
	return newWeightedAdmissionForJobsWeights(maxConcurrentJobs, DefaultSizeWeights)
}

// newWeightedAdmissionForJobsWeights is newWeightedAdmissionForJobs with the
// operator's size weights: the budget is maxConcurrentJobs M runs.
func newWeightedAdmissionForJobsWeights(maxConcurrentJobs int, weights SizeWeights) *weightedAdmission {
	if maxConcurrentJobs < 1 {
		maxConcurrentJobs = 1
	}
	gate := newWeightedAdmission(maxConcurrentJobs * weights.M)
	gate.weights = weights
	return gate
}

// concurrencyGroupKey scopes a declared group to its repository, so a document
// in one repository naming a group can never hold or wait on another
// repository's group of the same name.
func concurrencyGroupKey(repoID int64, group string) string {
	return strconv.FormatInt(repoID, 10) + "/" + group
}

// reserve records FIFO claim order before exact-SHA checkout and static
// parsing can finish out of order.
func (gate *weightedAdmission) reserve(sequence int64) *admissionReservation {
	return gate.reserveInOrder(sequence, sequence)
}

func (gate *weightedAdmission) reserveInOrder(sequence, order int64) *admissionReservation {
	return gate.reservePriorityRun(sequence, order, model.Run{})
}

func (gate *weightedAdmission) reservePriorityRun(sequence, order int64, run model.Run) *admissionReservation {
	if order <= 0 {
		order = sequence
	}
	reservation := &admissionReservation{gate: gate, sequence: sequence, order: order, ready: make(chan struct{}), claimedAt: time.Now().UTC()}
	reservation.runID, reservation.repoID, reservation.ref = run.ID, run.RepoID, run.Ref
	reservation.refKind, reservation.sha = run.RefKind, run.SHA
	reservation.priority, reservation.priorityReason, reservation.queuedAt = run.Priority, run.PriorityReason, run.QueuedAt
	reservation.priorityOverride, reservation.priorityOverrideReason = run.PriorityOverride, run.PriorityOverrideReason
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
func (gate *weightedAdmission) reserveRun(run model.Run) *admissionReservation {
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
	if a.order != b.order {
		return a.order < b.order
	}
	return a.sequence < b.sequence
}

// acquire admits an ungrouped run of the given size.
func (reservation *admissionReservation) acquire(ctx context.Context, size periapsis.Size) (func(), error) {
	return reservation.acquireRequest(ctx, admissionRequest{Size: size})
}

// acquireRequest sizes the reservation and waits until it is granted. A grouped
// run additionally waits until no other run of its repository holds the same
// concurrency group; while it does, it holds neither weight nor its place at
// the head of the queue, so it never delays the claims behind it.
func (reservation *admissionReservation) acquireRequest(ctx context.Context, request admissionRequest) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if !request.Size.Valid() {
		reservation.cancel()
		return nil, fmt.Errorf("service: invalid resource size %q", request.Size)
	}
	if request.Group != "" && !argoworkflow.ValidConcurrencyGroup(request.Group) {
		reservation.cancel()
		return nil, fmt.Errorf("service: invalid concurrency group %q", request.Group)
	}
	if request.KubeVirt != nil {
		if err := request.KubeVirt.Validate(); err != nil {
			reservation.cancel()
			return nil, err
		}
	}
	size := request.Size.Effective()
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
	reservation.vmParked = false
	if request.KubeVirt != nil {
		reservation.kubeVirt = *request.KubeVirt
	}
	if request.RunID != "" {
		reservation.runID = request.RunID
	}
	reservation.repoID, reservation.repo, reservation.ref = request.RepoID, request.Repo, request.Ref
	if request.RefKind != "" {
		reservation.refKind, reservation.sha = request.RefKind, request.SHA
	}
	reservation.estimate = request.Estimate
	if !request.HeadSince.IsZero() && (reservation.headSince.IsZero() || request.HeadSince.Before(reservation.headSince)) {
		reservation.headSince = request.HeadSince
	}
	if !reservation.prepared {
		reservation.preparedAt = time.Now().UTC()
	}
	reservation.size = size
	reservation.priorityMaximum = request.PriorityMaximum
	reservation.priorityLookupFailed = request.PriorityLookupFailed
	reservation.weight = gate.weights.of(size, gate.capacity)
	if request.Group != "" {
		reservation.group = concurrencyGroupKey(request.RepoID, request.Group)
		reservation.groupName = request.Group
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
// had not been granted: still preparing, waiting for weight, parked on its
// concurrency group, or parked on VM capacity. Such a run owns no Workflow,
// so nothing has to be proved gone first. Its place at the head of the FIFO,
// and its position ahead of later members of its group, end now rather than
// when its owner happens to wake, so the same pass admits the runs it was
// blocking. A granted reservation is never abandoned here: its owner releases
// it only once the Workflow is terminal or its deletion was accepted.
func (gate *weightedAdmission) abandon(sequence int64) bool {
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
	if !reservation.headSince.IsZero() {
		// The run is over: its head-wait origin must not be inherited by a
		// later run of the same commit.
		gate.emitHeadLocked(headEvent{key: reservation.headKey(), clear: true})
	}
	gate.grantLocked()
	return true
}

func (reservation *admissionReservation) release() {
	gate := reservation.gate
	gate.mu.Lock()
	defer gate.mu.Unlock()
	gate.releaseLocked(reservation)
}

// retain keeps a granted reservation's weight and concurrency group held
// until the process exits. The scheduler calls it only on the error paths
// where a Workflow may still be running and the owner is about to stop
// without having proved it terminal or deleted: passing the group on there
// would let the next member of the group start beside a live one. Startup
// reconciliation in the next process settles the Workflow; this process
// admits nothing further against the held weight.
func (reservation *admissionReservation) retain() {
	gate := reservation.gate
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if reservation.granted && !reservation.released {
		reservation.retained = true
	}
}

// grantLocked admits waiters in claim order. An unsized claim at the head
// holds its place, and so does a sized head that does not fit, which is what
// keeps heavy runs (and XL, which needs the whole node) from starving. Bounded
// backfill (#1085) relaxes the second rule for a while: until the head has
// waited maxHeadWait, later runs that fit the spare weight (and, when their
// history predicts it, finish before the bound) are admitted beside it; after
// the bound the pass ends at the head and the budget drains for it.
//
// A run parked on its concurrency group is the one exception. It is not
// waiting for the node but for another run of its own repository, so it is
// passed over rather than allowed to stop the pass: unrelated claims behind it
// are admitted on the weight budget as usual. Every later run of the same group
// is passed over too, which keeps the group itself in claim order, and the
// parked run regains its place in that order the moment its group is free.
func (gate *weightedAdmission) grantLocked() {
	// head is the first prepared, unparked waiter that does not fit. Before
	// #1085 it ended the pass; now later waiters that fit may be backfilled
	// beside it until it has waited maxHeadWait, after which the pass ends
	// at the head again so the budget drains for it.
	var head *admissionReservation
	now := gate.clock()
	gate.pressure.refresh(now)
	gate.sortWaitersLocked()
	gate.preparePressureWaitsLocked(now)
	for index := 0; index < len(gate.waiters); {
		reservation := gate.waiters[index]
		if reservation.vmParked || gate.kubeVirtBlockedLocked(reservation) {
			index++
			continue
		}
		if !reservation.prepared {
			// A claim whose size is still unknown holds its place at the
			// head. Behind a known head it is simply not a candidate yet.
			if head == nil {
				return
			}
			index++
			continue
		}
		if holder, ahead := gate.groupBlockerLocked(reservation); holder != "" || ahead != "" {
			index++
			continue
		}
		if !gate.canGrantLocked(reservation) {
			if head == nil {
				head = reservation
				gate.markHeadLocked(head, now)
				if !gate.backfillOpenLocked(head, now) {
					return
				}
			}
			index++
			continue
		}
		reservation.backfilled = false
		if head != nil {
			// Backfill is confined to the head's class; lower classes wait
			// for the higher class to drain even while its window is open.
			if reservation.effectivePriority(now) != head.effectivePriority(now) {
				return
			}
			if !gate.backfillFitsLocked(head, reservation, now) {
				index++
				continue
			}
			reservation.backfilled = true
		}
		// Stop at the first otherwise eligible candidate. A pressure timeout
		// never bypasses priority, group, VM, disk, weight or backfill rules.
		if gate.deferPressureLocked(reservation, now) {
			return
		}
		gate.waiters = append(gate.waiters[:index], gate.waiters[index+1:]...)
		reservation.granted = true
		reservation.grantedAt = time.Now().UTC()
		gate.used += reservation.weight
		gate.runs++
		if reservation.group != "" {
			gate.groups[reservation.group] = reservation
		}
		if !reservation.headSince.IsZero() {
			gate.emitHeadLocked(headEvent{key: reservation.headKey(), clear: true})
		}
		close(reservation.ready)
		if gate.onGrant != nil {
			gate.onGrant()
		}
	}
}

// spareWeightForClaim reports whether the dispatch loop may claim one more
// run past its MaxConcurrent ceiling: every claimed, unparked run holds its
// grant, and the weight budget, the run ceiling and the disk floor all still
// have room for at least an S run. unparked is the dispatch loop's count of
// claimed runs not parked on a group or VM capacity.
func (gate *weightedAdmission) spareWeightForClaim(unparked int) bool {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	// Every unparked claimed run must already hold its grant: one still
	// checking out or waiting for weight may need what is spare. The one
	// exception is a head that does not fit while its backfill window is
	// open (#1085): the spare weight is then exactly what backfill fills.
	waiting := len(gate.waiters) - gate.parkedLocked()
	extra := 0
	if waiting > 0 && gate.backfillClaimsOpenLocked(waiting) {
		// Every waiting claim is the head or a candidate already passed
		// over, so one more claim may be the run that fits.
		extra = waiting
	}
	if waiting-extra > 0 || unparked > gate.runs+extra || unparked >= gate.capacity {
		return false
	}
	if gate.maxRuns > 0 && gate.runs >= gate.maxRuns {
		return false
	}
	if gate.diskFloor != nil && !gate.diskFloor.freeAboveFloor() {
		return false
	}
	// Room for at least one S run, at the configured S weight.
	return gate.used+gate.weights.S <= gate.capacity
}

// canGrantLocked admits on the weight budget alone, with one exception: XL
// declares the whole node and therefore runs alone. When the gate mirrors a
// run ceiling (maxRuns), a grant also needs a free run slot.
//
// L carries no separate global exclusion. Its weight already prices it above
// the default size, so the budget throttles it proportionally and an operator
// who raises maxConcurrentJobs actually gets more concurrent large runs. A
// hard "one L at a time" rule instead pinned every L-declaring repository to a
// single run no matter what the operator configured.
func (gate *weightedAdmission) canGrantLocked(reservation *admissionReservation) bool {
	if gate.maxRuns > 0 && gate.runs >= gate.maxRuns {
		return false
	}
	// Disk floor hold (#673): when enabled and the filesystem is below the
	// configured floor, hold all new admissions until disk frees up. The
	// check uses a cached statfs result (sub-microsecond syscall, 5 s TTL)
	// so calling it under the lock is safe.
	if gate.diskFloor != nil && !gate.diskFloor.freeAboveFloor() {
		return false
	}
	// A whole-budget XL grant stays exclusive even if capacity is raised.
	for _, held := range gate.reservations {
		if held.granted && !held.released && gate.weights.wholeBudget(held.size) {
			return false
		}
	}
	if gate.weights.wholeBudget(reservation.size) {
		return gate.used == 0
	}
	return gate.used+reservation.weight <= gate.capacity
}

func (gate *weightedAdmission) releaseLocked(reservation *admissionReservation) {
	if !reservation.granted || reservation.released || reservation.retained {
		return
	}
	reservation.released = true
	delete(gate.reservations, reservation.sequence)
	gate.used -= reservation.weight
	gate.runs--
	if reservation.group != "" && gate.groups[reservation.group] == reservation {
		delete(gate.groups, reservation.group)
	}
	gate.grantLocked()
}

func (gate *weightedAdmission) removeLocked(reservation *admissionReservation) {
	delete(gate.reservations, reservation.sequence)
	for index, candidate := range gate.waiters {
		if candidate != reservation {
			continue
		}
		gate.waiters = append(gate.waiters[:index], gate.waiters[index+1:]...)
		return
	}
}

// parkedLocked counts VM-capacity waiters and prepared waiters held back by their concurrency
// group: another run of the same repository holds it, or an earlier run of the
// same group is waiting ahead of them.
func (gate *weightedAdmission) parkedLocked() int {
	parked := 0
	for _, reservation := range gate.waiters {
		if reservation.vmParked || gate.kubeVirtBlockedLocked(reservation) {
			parked++
			continue
		}
		if holder, ahead := gate.groupBlockerLocked(reservation); reservation.prepared && (holder != "" || ahead != "") {
			parked++
		}
	}
	return parked
}

// groupParked reports claimed runs parked on VM capacity or their concurrency
// group. The dispatch loop does not count them against its ceiling.
func (gate *weightedAdmission) groupParked() int {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	return gate.parkedLocked()
}

// groupBlockerLocked names what a waiting grouped reservation waits for: the
// run holding its group, or else the earlier run of the same group queued
// ahead of it. Both are empty when the reservation is not parked.
func (gate *weightedAdmission) groupBlockerLocked(reservation *admissionReservation) (holder, ahead string) {
	if reservation.group == "" || reservation.granted {
		return "", ""
	}
	if current := gate.groups[reservation.group]; current != nil {
		return current.runID, ""
	}
	for _, candidate := range gate.waiters {
		if candidate == reservation {
			break
		}
		if candidate.prepared && !candidate.vmParked && candidate.group == reservation.group {
			return "", candidate.runID
		}
	}
	return "", ""
}

// headBlockerLocked returns the waiter that stops the grant pass: the first
// claimed run in queue order that is prepared, not parked on its group or VM
// capacity, and still not granted. grantLocked has already admitted every
// waiter it could, so that run is waiting on weight, the run ceiling or the
// disk floor, and every unparked run claimed after it waits behind it. Nil
// while the head is still preparing (its size is unknown) or nothing waits.
func (gate *weightedAdmission) headBlockerLocked() *admissionReservation {
	for _, reservation := range gate.waiters {
		if reservation.vmParked || gate.kubeVirtBlockedLocked(reservation) {
			continue
		}
		if !reservation.prepared {
			return nil
		}
		if holder, ahead := gate.groupBlockerLocked(reservation); holder != "" || ahead != "" {
			continue
		}
		return reservation
	}
	return nil
}

func (gate *weightedAdmission) blockerLocked(reservation *admissionReservation) *model.AdmissionBlocker {
	blocker := &model.AdmissionBlocker{
		RunID: reservation.runID, RepoID: reservation.repoID, Repo: reservation.repo, Ref: reservation.ref,
		Size: string(reservation.size), Weight: reservation.weight, Used: gate.used, Capacity: gate.capacity,
		WaitingSince: reservation.preparedAt,
		Pressure:     gate.pressureWaitLocked(reservation),
	}
	switch {
	case gate.diskFloor != nil && !gate.diskFloor.freeAboveFloor():
		blocker.DiskFloor = true
	case gate.weights.wholeBudget(reservation.size):
		blocker.FullDrain = true
	case gate.maxRuns > 0 && gate.runs >= gate.maxRuns:
		blocker.RunCeiling = true
	}
	gate.backfillStateLocked(reservation, blocker)
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

// AdmissionSnapshot reports only this run's reservation and aggregate capacity.
// No reservation means unknown, never proof of admission or a failed scheduler.
func (scheduler *Scheduler) AdmissionSnapshot(sequence int64) *model.AdmissionObservation {
	gate := scheduler.admission
	gate.mu.Lock()
	defer gate.mu.Unlock()
	reservation := gate.reservations[sequence]
	if reservation == nil {
		return nil
	}
	result := &model.AdmissionObservation{State: "preparing", Used: gate.used, Capacity: gate.capacity}
	if reservation.prepared {
		result.State = "waiting"
		result.VMCapacity = reservation.vmParked
		result.KubeVirtCapacity = gate.kubeVirtBlockedLocked(reservation)
		result.Size = string(reservation.size)
		result.Weight = reservation.weight
		result.Group = reservation.groupName
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
	if reservation.prepared && !reservation.vmParked && !result.KubeVirtCapacity && result.GroupHolder == "" && result.GroupAhead == "" {
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

// parkVM is permitted only after the durable journal proves this run owns no
// execution. It preserves claim order but releases every scheduling permit.
func (reservation *admissionReservation) parkVM() error {
	gate := reservation.gate
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if !reservation.granted || reservation.released || reservation.canceled || reservation.retained {
		return fmt.Errorf("service: cannot park inactive VM reservation")
	}
	reservation.granted = false
	reservation.vmParked = true
	reservation.delayed = true
	reservation.ready = make(chan struct{})
	gate.used -= reservation.weight
	gate.runs--
	if reservation.group != "" && gate.groups[reservation.group] == reservation {
		delete(gate.groups, reservation.group)
	}
	gate.waiters = append(gate.waiters, reservation)
	gate.sortWaitersLocked()
	gate.grantLocked()
	if gate.onPark != nil {
		gate.onPark()
	}
	return nil
}

// Re-sort on every admission pass because starvation changes effective class.
func (gate *weightedAdmission) sortWaitersLocked() {
	now := gate.clock()
	sort.SliceStable(gate.waiters, func(i, j int) bool { return admissionBeforeAt(gate.waiters[i], gate.waiters[j], now) })
}
