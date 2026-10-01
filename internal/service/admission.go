package service

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"sync"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

type weightedAdmission struct {
	mu       sync.Mutex
	capacity int
	used     int
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
	// diskFloor is an opt-in disk-space admission hold (#673). When non-nil
	// and below the configured floor, no new runs are admitted until disk
	// frees up.
	diskFloor *diskFloorCheck
}

type admissionReservation struct {
	gate     *weightedAdmission
	sequence int64
	runID    string
	size     periapsis.Size
	weight   int
	// group is the repository-scoped key of the run's concurrency group and
	// groupName the name the repository declared; both empty when ungrouped.
	group     string
	groupName string
	prepared  bool
	granted   bool
	delayed   bool
	released  bool
	canceled  bool
	// retained marks a grant whose Workflow may still be running while its
	// owner stops on an error that leaves the run owned for the restart
	// pass. Its weight and group stay held for the rest of this process
	// instead of passing to the next run.
	retained bool
	ready    chan struct{}
}

// admissionRequest is what a claimed run knows once its immutable checkout has
// been read: its declared size and, optionally, its repository-scoped group.
type admissionRequest struct {
	RunID  string
	RepoID int64
	Size   periapsis.Size
	Group  string
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
	if maxConcurrentJobs < 1 {
		maxConcurrentJobs = 1
	}
	return newWeightedAdmission(maxConcurrentJobs * sizeWeight(periapsis.M))
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
	reservation := &admissionReservation{gate: gate, sequence: sequence, ready: make(chan struct{})}
	gate.mu.Lock()
	gate.reservations[sequence] = reservation
	gate.waiters = append(gate.waiters, reservation)
	sort.SliceStable(gate.waiters, func(i, j int) bool {
		return gate.waiters[i].sequence < gate.waiters[j].sequence
	})
	gate.grantLocked()
	gate.mu.Unlock()
	return reservation
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
	size := request.Size.Effective()
	gate := reservation.gate
	gate.mu.Lock()
	if reservation.canceled || reservation.released {
		gate.mu.Unlock()
		return nil, context.Canceled
	}
	reservation.runID = request.RunID
	reservation.size = size
	reservation.weight = min(sizeWeight(size), gate.capacity)
	if request.Group != "" {
		reservation.group = concurrencyGroupKey(request.RepoID, request.Group)
		reservation.groupName = request.Group
	}
	reservation.prepared = true
	gate.grantLocked()
	granted := reservation.granted
	reservation.delayed = !granted
	// Preparing is the only transition that can park a run: this one, or a
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

// grantLocked admits waiters in claim order. An unsized claim holds its place,
// and so does a sized run that does not fit: both stop the pass, which is what
// keeps heavy runs (and XL, which needs the whole node) from starving.
//
// A run parked on its concurrency group is the one exception. It is not
// waiting for the node but for another run of its own repository, so it is
// passed over rather than allowed to stop the pass: unrelated claims behind it
// are admitted on the weight budget as usual. Every later run of the same group
// is passed over too, which keeps the group itself in claim order, and the
// parked run regains its place in that order the moment its group is free.
func (gate *weightedAdmission) grantLocked() {
	var parked map[string]struct{}
	for index := 0; index < len(gate.waiters); {
		reservation := gate.waiters[index]
		if !reservation.prepared {
			return
		}
		if reservation.group != "" {
			_, held := gate.groups[reservation.group]
			_, behind := parked[reservation.group]
			if held || behind {
				if parked == nil {
					parked = make(map[string]struct{})
				}
				parked[reservation.group] = struct{}{}
				index++
				continue
			}
		}
		if !gate.canGrantLocked(reservation) {
			return
		}
		gate.waiters = append(gate.waiters[:index], gate.waiters[index+1:]...)
		reservation.granted = true
		gate.used += reservation.weight
		gate.runs++
		if reservation.group != "" {
			gate.groups[reservation.group] = reservation
		}
		close(reservation.ready)
	}
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
	if reservation.size == periapsis.XL {
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

// parkedLocked counts prepared waiters held back only by their own concurrency
// group: another run of the same repository holds it, or an earlier run of the
// same group is waiting ahead of them.
func (gate *weightedAdmission) parkedLocked() int {
	parked := 0
	var ahead map[string]struct{}
	for _, reservation := range gate.waiters {
		if !reservation.prepared || reservation.group == "" {
			continue
		}
		_, held := gate.groups[reservation.group]
		_, behind := ahead[reservation.group]
		if held || behind {
			parked++
		}
		if ahead == nil {
			ahead = make(map[string]struct{})
		}
		ahead[reservation.group] = struct{}{}
	}
	return parked
}

// groupParked reports how many claimed runs are parked on their concurrency
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
		if candidate.prepared && candidate.group == reservation.group {
			return "", candidate.runID
		}
	}
	return "", ""
}

func sizeWeight(size periapsis.Size) int {
	switch size.Effective() {
	case periapsis.S:
		return 1
	case periapsis.M:
		return 2
	case periapsis.L:
		return 3
	case periapsis.XL:
		return int(^uint(0) >> 1)
	default:
		return 0
	}
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
	result.GroupHolder, result.GroupAhead = gate.groupBlockerLocked(reservation)
	if gate.diskFloor != nil {
		result.DiskFloor = gate.diskFloor.observation()
	}
	return result
}
