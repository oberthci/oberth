package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/pkg/periapsis"
)

// groupRequest is one claimed run of repository repoID declaring group.
func groupRequest(runID string, repoID int64, size periapsis.Size, group string) admissionRequest {
	return admissionRequest{RunID: runID, RepoID: repoID, Size: size, Group: group}
}

func acquireRequestForTest(reservation *admissionReservation, request admissionRequest, acquired chan<- func()) {
	release, err := reservation.acquireRequest(context.Background(), request)
	if err == nil {
		acquired <- release
	}
}

func acquireRequestWithinBudget(t *testing.T, reservation *admissionReservation, request admissionRequest) func() {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), admissionAcquireBudget)
	defer cancel()
	release, err := reservation.acquireRequest(ctx, request)
	if err != nil {
		t.Fatalf("run %s (repo %d, group %q) was not admitted: %v", request.RunID, request.RepoID, request.Group, err)
	}
	return release
}

// At most one run of a (repository, group) holds a grant, however much weight
// is free: the group is a mutual exclusion, not a size.
func TestGroupAdmissionSerializesOneRepositoryGroup(t *testing.T) {
	gate := newWeightedAdmissionForJobs(6)
	holder := gate.reserve(1)
	releaseHolder := acquireRequestWithinBudget(t, holder, groupRequest("plan", 1, periapsis.M, "terraform-state"))

	waiter := gate.reserve(2)
	acquired := make(chan func(), 1)
	go acquireRequestForTest(waiter, groupRequest("apply", 1, periapsis.M, "terraform-state"), acquired)
	awaitWaiting(t, waiter)
	if used, runs := gateUsage(gate); used != 2 || runs != 1 {
		t.Fatalf("used=%d runs=%d; a run waiting on its group must hold no weight", used, runs)
	}
	releaseHolder()
	releaseWaiter := awaitAcquired(t, acquired)
	releaseWaiter()
}

// Different groups of one repository, and the same group name in different
// repositories, are independent: a group is scoped to its repository, so a
// document elsewhere naming "terraform-state" cannot block this one.
func TestGroupAdmissionIsPerGroupAndPerRepository(t *testing.T) {
	gate := newWeightedAdmissionForJobs(6)
	releases := []func(){
		acquireRequestWithinBudget(t, gate.reserve(1), groupRequest("tf-plan", 1, periapsis.M, "terraform-state")),
		acquireRequestWithinBudget(t, gate.reserve(2), groupRequest("tf-docs", 1, periapsis.M, "docs-site")),
		acquireRequestWithinBudget(t, gate.reserve(3), groupRequest("squatter", 2, periapsis.M, "terraform-state")),
		acquireRequestWithinBudget(t, gate.reserve(4), groupRequest("ungrouped", 1, periapsis.M, "")),
	}
	for _, release := range releases {
		release()
	}
	gate.mu.Lock()
	groups := len(gate.groups)
	gate.mu.Unlock()
	if used, runs := gateUsage(gate); groups != 0 || used != 0 || runs != 0 {
		t.Fatalf("released gate retains state: groups=%d used=%d runs=%d", groups, used, runs)
	}
}

// The reported defect was XL used as a mutex: every other repository waited
// behind it. A parked group run must never stop the pass: claims behind it are
// admitted on the weight budget while it waits for its own repository.
func TestGroupAdmissionParkedRunDoesNotBlockLaterClaims(t *testing.T) {
	gate := newWeightedAdmissionForJobs(3) // capacity 6
	releaseHolder := acquireRequestWithinBudget(t, gate.reserve(1), groupRequest("plan", 1, periapsis.M, "terraform-state"))

	parked := gate.reserve(2)
	parkedAcquired := make(chan func(), 1)
	go acquireRequestForTest(parked, groupRequest("second-plan", 1, periapsis.M, "terraform-state"), parkedAcquired)
	awaitWaiting(t, parked)

	// Another repository's M run, claimed after the parked run, is admitted.
	releaseOther := acquireRequestWithinBudget(t, gate.reserve(3), groupRequest("beacon", 2, periapsis.M, ""))
	if used, _ := gateUsage(gate); used != 4 {
		t.Fatalf("used = %d, want the holder and the other repository only (4)", used)
	}
	releaseOther()
	releaseHolder()
	releaseParked := awaitAcquired(t, parkedAcquired)
	releaseParked()
}

// Runs of one group are admitted in claim order: the group is FIFO, and a
// newer run of the group never overtakes an older one when the group frees.
func TestGroupAdmissionKeepsClaimOrderWithinTheGroup(t *testing.T) {
	gate := newWeightedAdmissionForJobs(6)
	releaseHolder := acquireRequestWithinBudget(t, gate.reserve(1), groupRequest("branch-a", 1, periapsis.M, "terraform-state"))

	older := gate.reserve(2)
	newer := gate.reserve(3)
	olderAcquired := make(chan func(), 1)
	newerAcquired := make(chan func(), 1)
	// The newer claim finishes sizing first; claim order still decides.
	go acquireRequestForTest(newer, groupRequest("apply-tag", 1, periapsis.M, "terraform-state"), newerAcquired)
	awaitWaiting(t, newer)
	go acquireRequestForTest(older, groupRequest("branch-b", 1, periapsis.M, "terraform-state"), olderAcquired)
	awaitWaiting(t, older)

	releaseHolder()
	releaseOlder := awaitAcquired(t, olderAcquired)
	awaitWaiting(t, newer)
	releaseOlder()
	releaseNewer := awaitAcquired(t, newerAcquired)
	releaseNewer()
}

// A group run whose group is free but whose weight does not fit keeps its place
// at the head exactly like any other sized run, so the group cannot be used to
// starve heavy runs or to jump the queue.
func TestGroupAdmissionFreeGroupStillQueuesOnWeight(t *testing.T) {
	gate := newWeightedAdmission(4)
	releaseBig := acquireRequestWithinBudget(t, gate.reserve(1), groupRequest("other", 2, periapsis.L, ""))

	grouped := gate.reserve(2)
	groupedAcquired := make(chan func(), 1)
	go acquireRequestForTest(grouped, groupRequest("plan", 1, periapsis.M, "terraform-state"), groupedAcquired)
	awaitWaiting(t, grouped)

	small := gate.reserve(3)
	smallAcquired := make(chan func(), 1)
	go acquireRequestForTest(small, groupRequest("small", 3, periapsis.S, ""), smallAcquired)
	awaitWaiting(t, small)

	releaseBig()
	releaseGrouped := awaitAcquired(t, groupedAcquired)
	releaseSmall := awaitAcquired(t, smallAcquired)
	releaseGrouped()
	releaseSmall()
}

// The group never changes the weight: a grouped run is priced at its declared
// size, and XL keeps its exclusivity whether or not it declares a group.
func TestGroupAdmissionPreservesDeclaredWeightAndXL(t *testing.T) {
	gate := newWeightedAdmissionForJobs(6)
	releaseLarge := acquireRequestWithinBudget(t, gate.reserve(1), groupRequest("plan", 1, periapsis.L, "terraform-state"))
	if used, _ := gateUsage(gate); used != sizeWeight(periapsis.L) {
		t.Fatalf("used = %d, want the declared L weight %d", used, sizeWeight(periapsis.L))
	}
	xl := gate.reserve(2)
	xlAcquired := make(chan func(), 1)
	go acquireRequestForTest(xl, groupRequest("kernel-matrix", 2, periapsis.XL, "vm-lab"), xlAcquired)
	awaitWaiting(t, xl)
	releaseLarge()
	releaseXL := awaitAcquired(t, xlAcquired)

	after := gate.reserve(3)
	afterAcquired := make(chan func(), 1)
	go acquireRequestForTest(after, groupRequest("tiny", 3, periapsis.S, ""), afterAcquired)
	awaitWaiting(t, after)
	releaseXL()
	releaseAfter := awaitAcquired(t, afterAcquired)
	releaseAfter()
}

// Parking is what the dispatch loop reads: runs held back only by their own
// group are counted, the first waiter of a free group is not, and a newly
// parked run wakes the dispatcher.
func TestGroupAdmissionCountsParkedRunsAndWakesTheDispatcher(t *testing.T) {
	gate := newWeightedAdmissionForJobs(6)
	wakes := make(chan struct{}, 8)
	gate.onPark = func() { wakes <- struct{}{} }

	releaseHolder := acquireRequestWithinBudget(t, gate.reserve(1), groupRequest("plan", 1, periapsis.M, "terraform-state"))
	if parked := gate.groupParked(); parked != 0 {
		t.Fatalf("parked = %d with only the holder", parked)
	}
	select {
	case <-wakes:
		t.Fatal("an admitted run woke the dispatcher as if parked")
	default:
	}

	first := gate.reserve(2)
	second := gate.reserve(3)
	firstAcquired := make(chan func(), 1)
	secondAcquired := make(chan func(), 1)
	go acquireRequestForTest(first, groupRequest("apply", 1, periapsis.M, "terraform-state"), firstAcquired)
	awaitWake(t, wakes)
	go acquireRequestForTest(second, groupRequest("plan-2", 1, periapsis.M, "terraform-state"), secondAcquired)
	awaitWake(t, wakes)
	if parked := gate.groupParked(); parked != 2 {
		t.Fatalf("parked = %d, want both runs behind the holder", parked)
	}

	releaseHolder()
	releaseFirst := awaitAcquired(t, firstAcquired)
	if parked := gate.groupParked(); parked != 1 {
		t.Fatalf("parked = %d, want only the run behind the new holder", parked)
	}
	releaseFirst()
	releaseSecond := awaitAcquired(t, secondAcquired)
	releaseSecond()
	if parked := gate.groupParked(); parked != 0 {
		t.Fatalf("parked = %d after the group drained", parked)
	}
}

// The scheduler mirrors its dispatch ceiling into the gate, so admitted runs
// never exceed MaxConcurrent even when parked runs let the loop claim more.
func TestGroupAdmissionMirroredRunCeiling(t *testing.T) {
	gate := newWeightedAdmissionForJobs(2) // capacity 4
	gate.maxRuns = 2
	releaseOne := acquireRequestWithinBudget(t, gate.reserve(1), groupRequest("one", 1, periapsis.S, ""))
	releaseTwo := acquireRequestWithinBudget(t, gate.reserve(2), groupRequest("two", 2, periapsis.S, ""))
	third := gate.reserve(3)
	thirdAcquired := make(chan func(), 1)
	go acquireRequestForTest(third, groupRequest("three", 3, periapsis.S, ""), thirdAcquired)
	awaitWaiting(t, third)
	releaseOne()
	releaseThird := awaitAcquired(t, thirdAcquired)
	releaseTwo()
	releaseThird()
}

// A canceled parked run leaves no trace, and a canceled holder frees the group.
func TestGroupAdmissionCancellationFreesTheGroup(t *testing.T) {
	gate := newWeightedAdmissionForJobs(6)
	holder := gate.reserve(1)
	acquireRequestWithinBudget(t, holder, groupRequest("plan", 1, periapsis.M, "terraform-state"))

	parkedCtx, cancelParked := context.WithCancel(context.Background())
	parked := gate.reserve(2)
	parkedDone := make(chan error, 1)
	go func() {
		_, err := parked.acquireRequest(parkedCtx, groupRequest("superseded", 1, periapsis.M, "terraform-state"))
		parkedDone <- err
	}()
	next := gate.reserve(3)
	nextAcquired := make(chan func(), 1)
	go acquireRequestForTest(next, groupRequest("newest", 1, periapsis.M, "terraform-state"), nextAcquired)
	awaitWaiting(t, next)
	cancelParked()
	if err := <-parkedDone; err == nil {
		t.Fatal("a canceled parked run was admitted")
	}
	awaitWaiting(t, next)
	holder.cancel()
	releaseNext := awaitAcquired(t, nextAcquired)
	releaseNext()
}

// A retained grant (its Workflow may still be alive while the owner stops)
// keeps its weight and its group through release and cancel alike.
func TestGroupAdmissionRetainedGrantKeepsItsGroupAndWeight(t *testing.T) {
	gate := newWeightedAdmissionForJobs(6)
	holder := gate.reserve(1)
	releaseHolder := acquireRequestWithinBudget(t, holder, groupRequest("plan", 1, periapsis.M, "terraform-state"))
	member := gate.reserve(2)
	memberCtx, cancelMember := context.WithCancel(context.Background())
	memberDone := make(chan error, 1)
	go func() {
		_, err := member.acquireRequest(memberCtx, groupRequest("apply", 1, periapsis.M, "terraform-state"))
		memberDone <- err
	}()
	awaitWaiting(t, member)

	holder.retain()
	releaseHolder()
	holder.cancel()
	awaitWaiting(t, member)
	if used, runs := gateUsage(gate); used != 2 || runs != 1 {
		t.Fatalf("used=%d runs=%d after releasing a retained grant; want its weight and run slot still held", used, runs)
	}

	// Retaining is only for a live grant: an ungranted or released
	// reservation is unaffected, so nothing can be retained by accident.
	unrelated := gate.reserve(3)
	releaseUnrelated := acquireRequestWithinBudget(t, unrelated, groupRequest("beacon", 2, periapsis.M, ""))
	releaseUnrelated()
	unrelated.retain()
	if used, runs := gateUsage(gate); used != 2 || runs != 1 {
		t.Fatalf("retaining a released grant changed the gate: used=%d runs=%d", used, runs)
	}
	cancelMember()
	if err := <-memberDone; err == nil {
		t.Fatal("the member was admitted while the retained holder kept the group")
	}
}

func TestGroupAdmissionRefusesAMalformedGroup(t *testing.T) {
	gate := newWeightedAdmissionForJobs(3)
	reservation := gate.reserve(1)
	if _, err := reservation.acquireRequest(context.Background(), groupRequest("bad", 1, periapsis.M, "Bad_Group")); err == nil ||
		!strings.Contains(err.Error(), "invalid concurrency group") {
		t.Fatalf("malformed group = %v", err)
	}
	gate.mu.Lock()
	queued := len(gate.waiters) + len(gate.reservations)
	gate.mu.Unlock()
	if queued != 0 {
		t.Fatal("a refused reservation stayed queued")
	}
}

// The #552 scheduling reason names the group and the exact run it waits on.
func TestGroupAdmissionSnapshotNamesTheGroupAndItsHolder(t *testing.T) {
	gate := newWeightedAdmissionForJobs(6)
	scheduler := &Scheduler{admission: gate}
	releaseHolder := acquireRequestWithinBudget(t, gate.reserve(1), groupRequest("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", 1, periapsis.M, "terraform-state"))
	if snapshot := scheduler.AdmissionSnapshot(1); snapshot.State != "admitted" || snapshot.Group != "terraform-state" ||
		snapshot.GroupHolder != "" || snapshot.GroupAhead != "" {
		t.Fatalf("holder snapshot = %#v", snapshot)
	}

	waiter := gate.reserve(2)
	acquired := make(chan func(), 1)
	go acquireRequestForTest(waiter, groupRequest("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", 1, periapsis.M, "terraform-state"), acquired)
	awaitWaiting(t, waiter)
	snapshot := scheduler.AdmissionSnapshot(2)
	if snapshot.State != "waiting" || snapshot.Group != "terraform-state" ||
		snapshot.GroupHolder != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" || snapshot.GroupAhead != "" || snapshot.Weight != 2 {
		t.Fatalf("waiting snapshot = %#v", snapshot)
	}
	message := waitingAdmissionMessage(snapshot)
	if !strings.Contains(message, `"terraform-state"`) || !strings.Contains(message, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa") {
		t.Fatalf("scheduling reason %q does not name the group and its holder", message)
	}
	releaseHolder()
	releaseWaiter := awaitAcquired(t, acquired)
	releaseWaiter()

	// With no holder, the run ahead in the group is named instead.
	releaseBig := acquireRequestWithinBudget(t, gate.reserve(3), groupRequest("big", 2, periapsis.XL, ""))
	ahead := gate.reserve(4)
	behind := gate.reserve(5)
	aheadAcquired := make(chan func(), 1)
	behindAcquired := make(chan func(), 1)
	go acquireRequestForTest(ahead, groupRequest("cccccccccccccccccccccccccccccccc", 1, periapsis.M, "terraform-state"), aheadAcquired)
	awaitWaiting(t, ahead)
	go acquireRequestForTest(behind, groupRequest("dddddddddddddddddddddddddddddddd", 1, periapsis.M, "terraform-state"), behindAcquired)
	awaitWaiting(t, behind)
	if snapshot := scheduler.AdmissionSnapshot(4); snapshot.GroupHolder != "" || snapshot.GroupAhead != "" {
		t.Fatalf("head of a free group waits on weight, not on the group: %#v", snapshot)
	}
	behindSnapshot := scheduler.AdmissionSnapshot(5)
	if behindSnapshot.GroupAhead != "cccccccccccccccccccccccccccccccc" || behindSnapshot.GroupHolder != "" {
		t.Fatalf("queued-behind snapshot = %#v", behindSnapshot)
	}
	if message := waitingAdmissionMessage(behindSnapshot); !strings.Contains(message, "behind run cccccccccccccccccccccccccccccccc") {
		t.Fatalf("queued-behind reason = %q", message)
	}
	if message := waitingAdmissionMessage(scheduler.AdmissionSnapshot(4)); message != "Waiting in the weighted resource admission queue." {
		t.Fatalf("ungrouped wait reason changed: %q", message)
	}
	releaseBig()
	releaseAhead := awaitAcquired(t, aheadAcquired)
	releaseAhead()
	releaseBehind := awaitAcquired(t, behindAcquired)
	releaseBehind()
}

// awaitWaiting waits until the acquiring goroutine has prepared the
// reservation, which is when the gate makes its admission decision for it, and
// then asserts that decision: waiting, neither granted nor canceled. Nothing
// else can grant it until the test itself releases or cancels a reservation,
// so this observes the gate's decision instead of inferring it from a quiet
// channel over a timer.
func awaitWaiting(t *testing.T, reservation *admissionReservation) {
	t.Helper()
	gate := reservation.gate
	deadline := time.Now().Add(admissionAcquireBudget)
	for {
		gate.mu.Lock()
		prepared, granted, canceled := reservation.prepared, reservation.granted, reservation.canceled
		gate.mu.Unlock()
		if prepared || canceled {
			if granted || canceled {
				t.Fatalf("reservation %d: granted=%v canceled=%v; want it waiting", reservation.sequence, granted, canceled)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("reservation %d was never prepared", reservation.sequence)
		}
		time.Sleep(time.Millisecond)
	}
}

// gateUsage reads the gate's weight and run counters under its lock.
func gateUsage(gate *weightedAdmission) (used, runs int) {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	return gate.used, gate.runs
}

func awaitWake(t *testing.T, wakes <-chan struct{}) {
	t.Helper()
	select {
	case <-wakes:
	case <-contextDeadline(t):
		t.Fatal("a parked run did not wake the dispatcher")
	}
}

func contextDeadline(t *testing.T) <-chan struct{} {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), admissionAcquireBudget)
	t.Cleanup(cancel)
	return ctx.Done()
}
