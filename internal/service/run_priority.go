package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/pkg/periapsis"
)

// QueuePriorityStore is an installation-wide admin capability. It is not
// exposed through scoped run dependencies.
type QueuePriorityStore interface {
	QueuePriorityPatterns(context.Context) ([]string, error)
	SetQueuePriorityPatterns(context.Context, string, []string) ([]string, error)
	BumpRunPriority(context.Context, string, string, model.RunPriority, string) (model.Run, error)
}

func (service *API) queuePriorityTool(ctx context.Context, actor api.Actor, name string, raw json.RawMessage) (any, error) {
	if !actor.Admin {
		return nil, refuse(ErrForbidden, "queue priority requires an admin uplink")
	}
	if err := requireUnscoped(actor, name); err != nil {
		return nil, err
	}
	if service.queuePriorities == nil {
		return nil, refuse(ErrUnavailable, "queue priority")
	}
	if name == "queue_priority_get" {
		if err := decodeTool(raw, &struct{}{}); err != nil {
			return nil, err
		}
		patterns, err := service.queuePriorities.QueuePriorityPatterns(ctx)
		return map[string]any{"rc_patterns": patterns, "starvation_minutes": 60}, err
	}
	if name == "queue_priority_config" {
		var args struct {
			Patterns *[]string `json:"rc_patterns"`
		}
		if err := decodeTool(raw, &args); err != nil {
			return nil, err
		}
		if args.Patterns == nil {
			return nil, refuse(ErrInvalidInput, "rc_patterns is required; use [] to disable")
		}
		patterns, err := service.queuePriorities.SetQueuePriorityPatterns(ctx, actor.Identity, *args.Patterns)
		if err != nil {
			return nil, err
		}
		if err := service.notifyPriorityChange(ctx); err != nil {
			return nil, err
		}
		return map[string]any{"rc_patterns": patterns, "starvation_minutes": 60}, nil
	}
	var args struct {
		RunID    string            `json:"run_id"`
		Priority model.RunPriority `json:"priority"`
		Reason   string            `json:"reason"`
	}
	if err := decodeTool(raw, &args); err != nil {
		return nil, err
	}
	bump := service.queuePriorities.BumpRunPriority
	if guarded, ok := service.enqueues.(interface {
		BumpRunPriority(context.Context, string, string, model.RunPriority, string) (model.Run, error)
	}); ok {
		bump = guarded.BumpRunPriority
	}
	run, err := bump(ctx, actor.Identity, args.RunID, args.Priority, args.Reason)
	if err != nil {
		return nil, err
	}
	if err := service.notifyPriorityChange(ctx); err != nil {
		return nil, err
	}
	return service.presentationRun(ctx, run), nil
}

func (service *API) notifyPriorityChange(ctx context.Context) error {
	if refresher, ok := service.enqueues.(interface{ RefreshQueuePriorities(context.Context) error }); ok {
		if err := refresher.RefreshQueuePriorities(ctx); err != nil {
			return fmt.Errorf("priority saved; refresh waiting reservations: %w", err)
		}
	}
	service.signals.NotifyRun("")
	if service.enqueues != nil {
		service.enqueues.NotifyQueue()
	}
	return nil
}

// BumpRunPriority holds the admission gate through the durable bump and its
// in-memory update. A weighted grant may precede started_at being persisted;
// refuse that window too, rather than auditing a bump of already granted work.
func (scheduler *Scheduler) BumpRunPriority(ctx context.Context, actor, id string, priority model.RunPriority, reason string) (model.Run, error) {
	state, ok := scheduler.store.(QueuePriorityStore)
	if !ok {
		return model.Run{}, refuse(ErrUnavailable, "queue priority")
	}
	scheduler.priorityMu.Lock()
	defer scheduler.priorityMu.Unlock()
	gate := scheduler.admission
	gate.mu.Lock()
	defer gate.mu.Unlock()
	for _, r := range gate.reservations {
		if r.runID == id && r.granted {
			return model.Run{}, fmt.Errorf("%w: run already holds an admission grant", store.ErrInvalidState)
		}
	}
	run, err := state.BumpRunPriority(ctx, actor, id, priority, reason)
	if err != nil {
		return model.Run{}, err
	}
	for _, r := range gate.waiters {
		if r.runID == id {
			r.priority, r.priorityReason = run.Priority, run.PriorityReason
			r.priorityOverride, r.priorityOverrideReason = run.PriorityOverride, run.PriorityOverrideReason
		}
	}
	gate.sortWaitersLocked()
	return run, nil
}

// priorityQueueReader keeps production priority support optional for small
// scheduler fakes. Both reads use current administrator state.
type priorityQueueReader interface {
	RunsByIDs(context.Context, []string) (map[string]model.Run, error)
	NextQueuedRun(context.Context) (model.Run, error)
}

// RefreshQueuePriorities applies admin changes to ungranted reservations and
// wakes dispatch. Existing resource grants are never preempted.
func (scheduler *Scheduler) RefreshQueuePriorities(ctx context.Context) error {
	defer scheduler.NotifyQueue()
	return scheduler.refreshPriorities(ctx)
}

// refreshPriorities reads outside the gate lock. Serialize readers so an older
// snapshot cannot overwrite a newer admin refresh.
func (scheduler *Scheduler) refreshPriorities(ctx context.Context) error {
	scheduler.priorityMu.Lock()
	defer scheduler.priorityMu.Unlock()
	reader, ok := scheduler.store.(priorityQueueReader)
	if !ok {
		return nil
	}
	gate := scheduler.admission
	gate.mu.Lock()
	ids := make([]string, 0, len(gate.waiters))
	for _, r := range gate.waiters {
		if r.runID != "" {
			ids = append(ids, r.runID)
		}
	}
	gate.mu.Unlock()
	if len(ids) == 0 {
		return nil
	}
	runs, err := reader.RunsByIDs(ctx, ids)
	if err != nil {
		return err
	}
	// Read grant state outside the gate as well. An unavailable size limit
	// drops automatic urgency but must never fail a release or abort refresh.
	maxima := make(map[int64]periapsis.Size)
	lookupFailed := make(map[int64]bool)
	for _, run := range runs {
		if _, seen := maxima[run.RepoID]; seen || run.RefKind != model.RefTag {
			continue
		}
		maximum, err := scheduler.releasePriorityMaximum(ctx, run)
		maxima[run.RepoID] = maximum
		lookupFailed[run.RepoID] = err != nil
	}
	gate.mu.Lock()
	defer gate.mu.Unlock()
	for _, r := range gate.waiters {
		if run, ok := runs[r.runID]; ok {
			r.priorityMaximum = maxima[run.RepoID]
			r.priorityLookupFailed = lookupFailed[run.RepoID]
			r.priority, r.priorityReason = run.Priority, run.PriorityReason
			r.priorityOverride, r.priorityOverrideReason = run.PriorityOverride, run.PriorityOverrideReason
		}
	}
	gate.sortWaitersLocked()
	return nil
}

// priorityDispatchAvailable admits at most one extra waiting claim per higher
// class, so a queue of ordinary claims cannot hide a release behind the
// dispatch ceiling. Granted work still owns its full budget/run permits.
func (scheduler *Scheduler) priorityDispatchAvailable(ctx context.Context, active int) (bool, error) {
	reader, ok := scheduler.store.(priorityQueueReader)
	if !ok {
		return false, nil
	}
	// Normal dispatch can already reach the weight budget plus parked claims.
	// Priority adds only the two non-normal classes.
	if active >= scheduler.admission.capacity+scheduler.maxConcurrent+2 {
		return false, nil
	}
	gate := scheduler.admission
	gate.mu.Lock()
	hasWaiters := len(gate.waiters) > 0
	gate.mu.Unlock()
	if !hasWaiters {
		return false, nil
	}
	next, err := reader.NextQueuedRun(ctx)
	if queueEmpty(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	gate.mu.Lock()
	defer gate.mu.Unlock()
	p, _ := next.EffectivePriority(gate.clock())
	candidate := &admissionReservation{gate: gate, sequence: next.QueueSequence, order: next.AdmissionSequence, priority: next.Priority, queuedAt: next.QueuedAt}
	lower := false
	for _, r := range gate.waiters {
		rp := r.effectivePriority(gate.clock())
		if rp.Rank() > p.Rank() || (rp.Rank() == p.Rank() && (p != model.PriorityHighest || !admissionBefore(candidate, r))) {
			return false, nil
		}
		lower = true
	}
	return lower, nil
}

func (r *admissionReservation) effectivePriority(now time.Time) model.RunPriority {
	run := r.priorityRun()
	p, _ := run.EffectivePriority(now)
	return p
}

// pollPriority is a bounded wake for aging and external configuration changes.
// The normal notification path applies local admin changes immediately.
const priorityPollInterval = 15 * time.Second

// priorityRun applies the exact-checkout size restriction every time priority
// is used, including after polling/admin refresh. Aging still applies normally.
func (r *admissionReservation) priorityRun() model.Run {
	run := model.Run{Priority: r.priority, PriorityReason: r.priorityReason, QueuedAt: r.queuedAt}
	maximum := r.priorityMaximum
	if maximum == "" {
		maximum = store.DefaultMaxRunSize
	}
	if run.PriorityReason == model.TagReleasePriorityReason && (r.priorityLookupFailed || (r.prepared && sizeRank(r.size) > sizeRank(maximum))) {
		run.Priority = model.PriorityNormal
		run.PriorityReason = fmt.Sprintf("tag/release automatic priority withheld: size %s exceeds repository maximum %s", r.size, maximum)
		if r.priorityLookupFailed {
			run.PriorityReason = "tag/release automatic priority withheld: repository size limit unavailable"
		}
		if r.priorityOverride != "" {
			run.Priority = r.priorityOverride
			run.PriorityReason = "admin bump: " + r.priorityOverrideReason
		}
	}
	return run
}

// releasePriorityMaximum is advisory: callers drop automatic urgency on error,
// never fail the run. Log the cause without depending on an audit DB write.
func (scheduler *Scheduler) releasePriorityMaximum(ctx context.Context, run model.Run) (periapsis.Size, error) {
	if limiter, ok := scheduler.store.(RunSizeLimiter); ok && run.RefKind == model.RefTag {
		maximum, err := limiter.MaxRunSize(ctx, run.RepoID)
		if err != nil {
			log.Printf("WARN: release priority withheld run=%s repo_id=%d reason=size_limit_unavailable error=%v", run.ID, run.RepoID, err)
			return "", fmt.Errorf("read release priority size limit: %w", err)
		}
		return maximum, nil
	}
	return store.DefaultMaxRunSize, nil
}

// AdmissionPriority reports the gate's exact-checkout priority for run_get and
// scheduling messages; before checkout the durable classification is provisional.
func (scheduler *Scheduler) AdmissionPriority(sequence int64) (model.RunPriority, string, bool) {
	gate := scheduler.admission
	gate.mu.Lock()
	defer gate.mu.Unlock()
	r := gate.reservations[sequence]
	if r == nil || !r.prepared {
		return "", "", false
	}
	run := r.priorityRun()
	return run.Priority, run.PriorityReason, true
}

func (service *API) withAdmissionPriority(run model.Run) model.Run {
	if observer, ok := service.admission.(interface {
		AdmissionPriority(int64) (model.RunPriority, string, bool)
	}); ok {
		if priority, reason, found := observer.AdmissionPriority(run.QueueSequence); found {
			run.Priority, run.PriorityReason = priority, reason
		}
	}
	return run
}
