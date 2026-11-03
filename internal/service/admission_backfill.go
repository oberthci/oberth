package service

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/oberthci/oberth/internal/liveness"
	"github.com/oberthci/oberth/internal/model"
)

// DefaultMaxHeadWait marks when the queue reports draining for its head.
// Only measured backfill preserving the head's reservation is admitted.
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
	key   headWaitKey
	since time.Time
	clear bool
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
	select {
	case gate.headEvents <- event:
	default:
		// Persistence is best effort: a dropped event costs only the origin
		// surviving a restart, never admission itself.
	}
}

// backfillEnabledLocked reports whether runs behind this head may still be
// admitted beside it.
func (gate *weightedAdmission) backfillEnabledLocked(head *admissionReservation) bool {
	return gate.maxHeadWait > 0 && !head.headSince.IsZero()
}

// backfillFitsLocked is the single EASY rule, for every priority class and
// on both sides of maxHeadWait. The caller checks current capacity first.
// A measured candidate must finish strictly before the reserved head start,
// or leave room for the head at that time while still running.
func (gate *weightedAdmission) backfillFitsLocked(head, candidate *admissionReservation, now time.Time) bool {
	candidate.backfillMessage = "held: would delay head"
	if candidate.estimate <= 0 {
		candidate.backfillMessage += " (no measured p95 duration)"
		return false
	}
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
		if now.Add(candidate.estimate).Before(deadline) {
			candidate.backfillMessage = fmt.Sprintf("backfilled: ends before head can start (p95 %s < %s)",
				backfillDuration(candidate.estimate), backfillDuration(deadline.Sub(now)))
			return true
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

func backfillDuration(d time.Duration) string {
	return fmt.Sprintf("%gm", d.Minutes())
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
	if gate.maxHeadWait <= 0 || head.headSince.IsZero() || !gate.priorityHeadFitsAlongsideLocked(head, nil) {
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
	if canSweep {
		_ = sweeper.SweepAdmissionHeadWaits(ctx)
	}
	for {
		scheduler.headProgress.Beat()
		select {
		case <-ctx.Done():
			return
		case <-pulse.C:
		case <-sweep.C:
			// Heads whose run ended without being abandoned through the gate
			// (an interrupted promotion or tag) leave rows behind.
			if canSweep {
				_ = sweeper.SweepAdmissionHeadWaits(ctx)
			}
		case event := <-events:
			key := event.key
			persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			if event.clear {
				_ = store.ClearAdmissionHeadWait(persistCtx, key.RepoID, key.RefKind, key.Ref, key.SHA)
			} else {
				_ = store.RecordAdmissionHeadWait(persistCtx, key.RepoID, key.RefKind, key.Ref, key.SHA, event.since)
			}
			cancel()
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
		if !reservation.vmParked && !reservation.prepared {
			return false
		}
	}
	return true
}
