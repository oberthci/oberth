package service

import (
	"context"
	"fmt"
	"log"
	"sort"
	"time"

	"github.com/oberthci/oberth/internal/liveness"
	"github.com/oberthci/oberth/internal/model"
)

// DefaultMaxHeadWait marks when the queue reports draining for its head.
// Backfill must preserve the head's reservation.
const DefaultMaxHeadWait = 20 * time.Minute

// headWaitKey identifies a run's head-wait origin across a restart: a
// requeued run is a copy with a new ID but the same repository, ref and
// commit.
type headWaitKey struct {
	RepoID  int64
	RefKind model.RefKind
	Ref     string
	SHA     string
}

type headEvent struct {
	coalesced bool
	key       headWaitKey
	since     time.Time
	clear     bool
	// A clear followed by a record must still delete the previous generation
	// before inserting: the store deliberately keeps an existing first origin.
	clearFirst bool
}

// coalesceHeadEvent composes changes for one key in emission order. Keep the
// first origin within a generation and every clear that separates generations.
func coalesceHeadEvent(previous, next headEvent) headEvent {
	if next.clear || next.clearFirst {
		return next
	}
	next.clearFirst = previous.clear || previous.clearFirst
	if !previous.clear && !previous.since.IsZero() && (next.since.IsZero() || previous.since.Before(next.since)) {
		next.since = previous.since
	}
	return next
}

// HeadWaitStore persists head-wait origins (#1085). The production store
// implements it; without it the origin lives only as long as the process.
type HeadWaitStore interface {
	AdmissionHeadWait(ctx context.Context, repoID int64, refKind model.RefKind, ref, sha string) (time.Time, bool, error)
	RecordAdmissionHeadWait(ctx context.Context, repoID int64, refKind model.RefKind, ref, sha string, since time.Time) error
	ClearAdmissionHeadWait(ctx context.Context, repoID int64, refKind model.RefKind, ref, sha string) error
}

// durationClass is the history a run is estimated from: release (tag) runs,
// promotions, schedules and branch runs are separate classes, so a release
// is never predicted from branch CI.
func durationClass(run model.Run) string {
	switch {
	case run.Release:
		return "release"
	case run.Trigger == "promotion" || run.Trigger == "schedule":
		return run.Trigger
	default:
		return "branch"
	}
}

func (reservation *admissionReservation) headKey() headWaitKey {
	return headWaitKey{RepoID: reservation.repoID, RefKind: reservation.refKind, Ref: reservation.ref, SHA: reservation.sha}
}

func (gate *weightedAdmission) clock() time.Time {
	if gate.now != nil {
		return gate.now()
	}
	return time.Now()
}

// A displaced head gets a fresh forecast when it returns, while its durable
// wait origin remains unchanged. Overruns cannot extend a continuous head's
// reservation and therefore stop new backfill until that head starts.
func (gate *weightedAdmission) setBackfillHeadLocked(head *admissionReservation) {
	if gate.backfillHead == head {
		return
	}
	if gate.backfillHead != nil {
		gate.backfillHead.backfillDeadline = time.Time{}
	}
	gate.backfillHead = head
	if head != nil {
		head.backfillDeadline = time.Time{}
	}
}

// markHeadLocked dates the first time a reservation waits at the head and
// asks for the origin to be persisted.
func (gate *weightedAdmission) markHeadLocked(head *admissionReservation, now time.Time) {
	if gate.maxHeadWait <= 0 || !head.headSince.IsZero() {
		return
	}
	head.headSince = now
	gate.emitHeadLocked(headEvent{key: head.headKey(), since: now})
}

func (gate *weightedAdmission) emitHeadLocked(event headEvent) {
	if gate.headEvents == nil || event.key.RepoID <= 0 || event.key.SHA == "" {
		return
	}
	if gate.headPending == nil {
		gate.headPending = make(map[headWaitKey]headEvent)
	}
	event.coalesced = true
	if previous, found := gate.headPending[event.key]; found {
		event = coalesceHeadEvent(previous, event)
	}
	gate.headPending[event.key] = event
	select {
	case gate.headEvents <- event:
	default:
		// The channel is only a wake-up. The latest change for every key stays
		// in headPending, including clears, even when notifications coalesce.
	}
}

// backfillEnabledLocked reports whether runs behind this head may still be
// admitted beside it.
func (gate *weightedAdmission) backfillEnabledLocked(head *admissionReservation) bool {
	return gate.maxHeadWait > 0 && !head.headSince.IsZero()
}

// backfillFitsLocked is the single EASY rule, for every priority class and
// on both sides of maxHeadWait. The caller checks current capacity first.
// A candidate must finish strictly before the reserved head start, or leave
// room for the head at that time. Unmeasured candidates use coexistence only.
func (gate *weightedAdmission) backfillFitsLocked(head, candidate *admissionReservation, now time.Time) bool {
	candidate.backfillMessage = "held: would delay head"
	projection := gate.backfillProjectionLocked()
	var completions []time.Time
	for _, r := range projection.reservations {
		if finish, ok := expectedFinish(r, now); ok {
			completions = append(completions, finish)
		}
	}
	sort.Slice(completions, func(i, j int) bool { return completions[i].Before(completions[j]) })
	for _, finish := range completions {
		projection.finishBy(finish, now)
		if !projection.canGrantLocked(head) || !projection.kubeVirtFitsLocked(head) {
			continue
		}
		// Later backfills cannot move an existing reservation into the future.
		if head.backfillDeadline.IsZero() || finish.Before(head.backfillDeadline) {
			head.backfillDeadline = finish
		}
		deadline := head.backfillDeadline
		if !now.Before(deadline) {
			return false
		}
		if candidate.estimate > 0 && now.Add(candidate.estimate).Before(deadline) {
			candidate.backfillMessage = fmt.Sprintf("backfilled: ends before head can start (p95 %dm < %dm)",
				(candidate.estimate-1)/time.Minute+1, deadline.Sub(now)/time.Minute)
			return true
		}
		// Coexistence protects only this head. A lower effective or base
		// class must finish first, so it cannot occupy room needed by any
		// higher-class waiter ahead of it when the head starts.
		if candidate.effectivePriority(now) != head.effectivePriority(now) ||
			candidate.priorityRun().Priority != head.priorityRun().Priority {
			return false
		}
		// Rebuild at the reserved time, including every surviving grant. This
		// checks aggregate backfills, mixed declarations/weights, XL exclusion,
		// memory, executor allowances and both run ceilings through one path.
		projection = gate.backfillProjectionLocked()
		projection.finishBy(deadline, now)
		projection.used += candidate.weight
		projection.runs++
		// numericFitsLocked accounts only grants; use a copy of the candidate.
		held := *candidate
		held.granted, held.released = true, false
		projection.reservations[candidate.sequence] = &held
		if projection.canGrantLocked(head) && projection.kubeVirtFitsLocked(head) {
			candidate.backfillMessage = "backfilled: head still fits at its reserved start"
			return true
		}
		return false
	}
	candidate.backfillMessage += " (running p95 finish unknown)"
	return false
}

// Unknown, retained and overrun work is never assumed to finish now. Treating
// max(0, p95-elapsed) as an immediate release would create fictitious capacity.
func expectedFinish(r *admissionReservation, now time.Time) (time.Time, bool) {
	finish := r.grantedAt.Add(r.estimate)
	return finish, r.estimate > 0 && !r.grantedAt.IsZero() && !r.retained && finish.After(now)
}

func (gate *weightedAdmission) backfillProjectionLocked() *weightedAdmission {
	p := &weightedAdmission{resources: gate.resources, capacity: gate.capacity, weights: gate.weights,
		maxRuns: gate.maxRuns, reservations: make(map[int64]*admissionReservation)}
	for seq, r := range gate.reservations {
		if r.granted && !r.released {
			p.reservations[seq] = r
			p.used += r.weight
			p.runs++
		}
	}
	return p
}

func (gate *weightedAdmission) finishBy(deadline, now time.Time) {
	for seq, r := range gate.reservations {
		if finish, ok := expectedFinish(r, now); ok && !finish.After(deadline) {
			delete(gate.reservations, seq)
			gate.used -= r.weight
			gate.runs--
		}
	}
}

// backfillStateLocked describes the head's backfill window for the
// admission view and messages.
func (gate *weightedAdmission) backfillStateLocked(head *admissionReservation, blocker *model.AdmissionBlocker) {
	if gate.maxHeadWait <= 0 || head.headSince.IsZero() {
		return
	}
	now := gate.clock()
	blocker.HeadSince = head.headSince
	blocker.HeadWaitSeconds = int64(now.Sub(head.headSince) / time.Second)
	blocker.MaxHeadWaitSeconds = int64(gate.maxHeadWait / time.Second)
	if now.Sub(head.headSince) < gate.maxHeadWait {
		blocker.Backfilling = true
	} else {
		blocker.Draining = true
	}
}

// runHeadEvents persists head-wait origins in order until ctx ends.
func (scheduler *Scheduler) runHeadEvents(ctx context.Context) {
	store, ok := scheduler.store.(HeadWaitStore)
	events := scheduler.admission.headEvents
	if !ok || events == nil {
		return
	}
	sweeper, canSweep := store.(interface {
		SweepAdmissionHeadWaits(context.Context) error
	})
	scheduler.headProgress.Beat()
	pulse := time.NewTicker(liveness.Interval)
	defer pulse.Stop()
	sweep := time.NewTicker(time.Hour)
	defer sweep.Stop()
	// Retry failed writes independently of the heartbeat and hourly cleanup.
	// A nil channel disables the timer once every pending event is durable.
	retry := time.NewTimer(30 * time.Second)
	retry.Stop()
	defer retry.Stop()
	var retryC <-chan time.Time
	if canSweep {
		if err := sweeper.SweepAdmissionHeadWaits(ctx); err != nil {
			log.Printf("WARN: admission head-wait sweep: %v", err)
		}
	}
	for {
		scheduler.headProgress.Beat()
		var direct *headEvent
		select {
		case <-ctx.Done():
			return
		case <-pulse.C:
			continue
		case <-retryC:
			retryC = nil
		case <-sweep.C:
			// Heads whose run ended without being abandoned through the gate
			// (an interrupted promotion or tag) leave rows behind.
			if canSweep {
				if err := sweeper.SweepAdmissionHeadWaits(ctx); err != nil {
					log.Printf("WARN: admission head-wait sweep: %v", err)
				}
			}
		case event := <-events:
			if !event.coalesced {
				direct = &event
			}
		}
		gate := scheduler.admission
		gate.mu.Lock()
		batch := gate.headPending
		gate.headPending = nil
		gate.mu.Unlock()
		if direct != nil {
			if batch == nil {
				batch = make(map[headWaitKey]headEvent)
			}
			if newer, found := batch[direct.key]; found {
				batch[direct.key] = coalesceHeadEvent(*direct, newer)
			} else {
				batch[direct.key] = *direct
			}
		}
		for _, event := range batch {
			key := event.key
			persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			var err error
			if event.clear || event.clearFirst {
				err = store.ClearAdmissionHeadWait(persistCtx, key.RepoID, key.RefKind, key.Ref, key.SHA)
			}
			if err == nil && !event.clear {
				err = store.RecordAdmissionHeadWait(persistCtx, key.RepoID, key.RefKind, key.Ref, key.SHA, event.since)
			}
			cancel()
			if err != nil {
				log.Printf("WARN: admission head-wait persistence repo=%d clear=%t: %v", key.RepoID, event.clear, err)
				gate.mu.Lock()
				if gate.headPending == nil {
					gate.headPending = make(map[headWaitKey]headEvent)
				}
				if newer, found := gate.headPending[key]; found {
					gate.headPending[key] = coalesceHeadEvent(event, newer)
				} else {
					gate.headPending[key] = event
				}
				gate.mu.Unlock()
			}
		}
		gate.mu.Lock()
		pending := len(gate.headPending) > 0
		gate.mu.Unlock()
		if pending && retryC == nil {
			retry.Reset(30 * time.Second)
			retryC = retry.C
		} else if !pending && retryC != nil {
			retry.Stop()
			retryC = nil
		}
	}
}

// admissionHistory reads a claimed run's durable head-wait origin and its
// predicted duration before it is sized into the gate.
func (scheduler *Scheduler) admissionHistory(ctx context.Context, run model.Run) (time.Time, time.Duration) {
	var since time.Time
	if store, ok := scheduler.store.(HeadWaitStore); ok {
		if recorded, found, err := store.AdmissionHeadWait(ctx, run.RepoID, run.RefKind, run.Ref, run.SHA); err == nil && found {
			since = recorded
		}
	}
	return since, scheduler.measuredBackfillDuration(ctx, run)
}

// backfillClaimsOpenLocked reports whether the dispatch loop may claim past
// runs that are waiting only because they are the head or were passed over
// as backfill candidates, including while draining. A claim still
// preparing could itself fit, so none may be in flight; the waiting claims
// are bounded by the budget's M-run count, and the caller's run, budget and
// disk checks still apply.
func (gate *weightedAdmission) backfillClaimsOpenLocked(waiting int) bool {
	head := gate.headBlockerLocked()
	if head == nil || !gate.backfillEnabledLocked(head) {
		return false
	}
	if limit := gate.capacity / max(gate.weights.M, 1); waiting > limit {
		return false
	}
	for _, reservation := range gate.waiters {
		if !reservation.prepared {
			return false
		}
	}
	return true
}
