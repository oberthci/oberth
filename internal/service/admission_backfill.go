package service

import (
	"context"
	"log"
	"time"

	"github.com/oberthci/oberth/internal/liveness"
	"github.com/oberthci/oberth/internal/model"
)

// DefaultMaxHeadWait is how long a queue head that does not fit lets later
// runs be backfilled beside it before the budget drains for it (#1085).
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

// RunDurationEstimator predicts a run's duration from the repository's
// history of the same class (#1085); ok is false without enough history. The
// prediction is advisory: it only narrows which runs are backfilled.
type RunDurationEstimator interface {
	RunDurationEstimate(ctx context.Context, repoID int64, class string) (time.Duration, bool, error)
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

// backfillOpenLocked reports whether runs behind this head may still be
// admitted beside it.
func (gate *weightedAdmission) backfillOpenLocked(head *admissionReservation, now time.Time) bool {
	return !gate.numericBlockedLocked(head) && gate.maxHeadWait > 0 && !head.headSince.IsZero() && now.Sub(head.headSince) < gate.maxHeadWait
}

// backfillFitsLocked reports whether a run that fits the spare weight may be
// backfilled: when history predicts its duration, it must be predicted to
// finish before the head's bound. A run without history is admitted only
// while the bound has not passed (which backfillOpenLocked already checks).
func (gate *weightedAdmission) backfillFitsLocked(head, candidate *admissionReservation, now time.Time) bool {
	if !gate.priorityHeadFitsAlongsideLocked(head, candidate) || gate.numericBlockedLocked(head) {
		// Spare weight cannot bypass priority or CPU/memory headroom guards.
		return false
	}
	if candidate.estimate <= 0 {
		return true
	}
	return !now.Add(candidate.estimate).After(head.headSince.Add(gate.maxHeadWait))
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
	if gate.backfillOpenLocked(head, now) {
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
	var estimate time.Duration
	if estimator, ok := scheduler.store.(RunDurationEstimator); ok {
		if predicted, found, err := estimator.RunDurationEstimate(ctx, run.RepoID, durationClass(run)); err == nil && found {
			estimate = predicted
		}
	}
	return since, estimate
}

// backfillClaimsOpenLocked reports whether the dispatch loop may claim past
// runs that are waiting only because they are the head or were passed over
// as backfill candidates, while the head's window is open. A claim still
// preparing could itself fit, so none may be in flight; the waiting claims
// are bounded by the budget's M-run count, and the caller's run, budget and
// disk checks still apply.
func (gate *weightedAdmission) backfillClaimsOpenLocked(waiting int) bool {
	head := gate.headBlockerLocked()
	if head == nil || !gate.backfillOpenLocked(head, gate.clock()) {
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
