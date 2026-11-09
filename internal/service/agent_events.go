package service

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/pushretry"
)

// The agent event recorder (#1514) turns durable state into the fixed event
// vocabulary of model.AgentEvent. It reacts to the existing run and
// promotion change signals and sweeps on an interval for what has no signal:
// parked retry holds, native Argo lock waits and publication retries. Every
// event is deduplicated by a key, so a restart or a second observation never
// records one fact twice. It reads and records only; the one action it takes
// is a promotion's prune_source request, which its promoter asked for.

// AgentEventStore is the durable side of the recorder.
type AgentEventStore interface {
	AppendAgentEvent(context.Context, model.AgentEvent, int) (model.AgentEvent, bool, error)
	AgentEvents(context.Context, model.AgentEventFilter) ([]model.AgentEvent, error)
	AgentEventBounds(context.Context) (int64, int64, error)
	RecordSyncObservation(context.Context, time.Time, []model.SyncParticipant, map[string]bool, map[string]bool, int) error
	SyncIntervals(context.Context, model.SyncIntervalFilter) ([]model.SyncInterval, error)
	RequestPromotionPrune(context.Context, string, string) error
	PromotionPrune(context.Context, string) (model.PromotionPrune, error)
	PendingPromotionPrunes(context.Context, int) ([]model.PromotionPrune, error)
	FinishPromotionPrune(context.Context, string, string, string, string) (model.PromotionPrune, error)
}

// SynchronizationObserver reads one run's native Argo lock state.
type SynchronizationObserver interface {
	ObserveSynchronization(context.Context, model.Run) (*model.SyncObservation, error)
}

// AgentEventPolicy is the administrator bound of the recorder. Every field is
// a Helm value (agentEvents.*) rendered into a serve flag.
type AgentEventPolicy struct {
	// Retention keeps this many newest events (events_since history).
	Retention int
	// SyncRetention keeps this many newest native lock intervals.
	SyncRetention int
	// Interval is the sweep period for holds, lock waits and retries.
	Interval time.Duration
	// LockWait is how long a run waits on one lock before run.lock_wait.
	LockWait time.Duration
}

// Agent event defaults and bounds.
const (
	DefaultAgentEventRetention     = 10000
	MaxAgentEventRetention         = 1000000
	DefaultAgentSyncRetention      = 20000
	MaxAgentSyncRetention          = 1000000
	DefaultAgentEventInterval      = 10 * time.Second
	MinAgentEventInterval          = 2 * time.Second
	MaxAgentEventInterval          = 5 * time.Minute
	DefaultAgentEventLockWait      = time.Minute
	agentEventLookback             = 15 * time.Minute
	agentEventMaxObservedRuns      = 64
	agentEventMemoLimit            = 8192
	agentEventPruneGiveUp          = time.Hour
	agentEventSummaryErrorLength   = 200
	agentEventDataErrorLength      = 600
	agentEventPendingNotifications = 1024
)

// Effective applies defaults and clamps.
func (policy AgentEventPolicy) Effective() AgentEventPolicy {
	if policy.Retention <= 0 {
		policy.Retention = DefaultAgentEventRetention
	}
	policy.Retention = min(policy.Retention, MaxAgentEventRetention)
	if policy.SyncRetention <= 0 {
		policy.SyncRetention = DefaultAgentSyncRetention
	}
	policy.SyncRetention = min(policy.SyncRetention, MaxAgentSyncRetention)
	if policy.Interval <= 0 {
		policy.Interval = DefaultAgentEventInterval
	}
	policy.Interval = min(max(policy.Interval, MinAgentEventInterval), MaxAgentEventInterval)
	if policy.LockWait <= 0 {
		policy.LockWait = DefaultAgentEventLockWait
	}
	return policy
}

type agentNotification struct{ kind, id string }

type agentEventRecorder struct {
	base    *API
	store   AgentEventStore
	policy  AgentEventPolicy
	started time.Time
	now     func() time.Time
	pending chan agentNotification
	wakeMu  sync.Mutex
	wake    chan struct{}
	// memo skips keys this process already recorded; bounded and reset.
	memo map[string]bool
	// snapshot is the last native lock observation, for sync_status.
	snapshotMu sync.Mutex
	snapshot   syncSnapshot
	once       sync.Once
}

type syncSnapshot struct {
	ObservedAt   time.Time
	Participants []model.SyncParticipant
	Unobserved   int
}

func newAgentEventRecorder(base *API, store AgentEventStore, policy AgentEventPolicy) *agentEventRecorder {
	if store == nil {
		return nil
	}
	recorder := &agentEventRecorder{base: base, store: store, policy: policy.Effective(), now: time.Now,
		pending: make(chan agentNotification, agentEventPendingNotifications), wake: make(chan struct{}), memo: map[string]bool{}}
	base.signals.Observe(recorder.notify)
	return recorder
}

// notify never blocks a signal: a full queue is covered by the next sweep.
func (recorder *agentEventRecorder) notify(kind, id string) {
	select {
	case recorder.pending <- agentNotification{kind: kind, id: id}:
	default:
	}
}

func (recorder *agentEventRecorder) wakeChannel() <-chan struct{} {
	recorder.wakeMu.Lock()
	defer recorder.wakeMu.Unlock()
	return recorder.wake
}

func (recorder *agentEventRecorder) broadcast() {
	recorder.wakeMu.Lock()
	defer recorder.wakeMu.Unlock()
	close(recorder.wake)
	recorder.wake = make(chan struct{})
}

// StartAgentEvents runs the recorder until ctx ends. It is started once,
// after owner activation, so it never writes before recovery finished. It
// never fails; the error result lets activation return it directly.
func (service *API) StartAgentEvents(ctx context.Context) error {
	recorder := service.agentEvents
	if recorder == nil {
		return nil
	}
	recorder.once.Do(func() {
		if !service.startBackground() {
			return
		}
		go func() {
			defer service.background.Done()
			recorder.run(ctx)
		}()
	})
	return nil
}

func (recorder *agentEventRecorder) run(ctx context.Context) {
	recorder.started = recorder.now()
	ticker := time.NewTicker(recorder.policy.Interval)
	defer ticker.Stop()
	recorder.sweep(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-recorder.base.lifetime.Done():
			return
		case notification := <-recorder.pending:
			recorder.handle(ctx, notification)
		case <-ticker.C:
			recorder.sweep(ctx)
		}
	}
}

func (recorder *agentEventRecorder) handle(ctx context.Context, notification agentNotification) {
	readCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	switch notification.kind {
	case "run":
		if recorder.base.history == nil {
			return
		}
		run, err := recorder.base.history.Run(readCtx, notification.id)
		if err == nil {
			recorder.observeRun(readCtx, run)
		}
	case "promotion":
		if recorder.base.promotions == nil {
			return
		}
		promotion, err := recorder.base.promotions.Promotion(readCtx, notification.id)
		if err == nil {
			recorder.observePromotion(readCtx, promotion)
		}
	}
}

// sweep observes what has no change signal, and repeats recent terminal
// facts so a lost signal or a restart cannot drop an event.
func (recorder *agentEventRecorder) sweep(ctx context.Context) {
	sweepCtx, cancel := context.WithTimeout(ctx, max(recorder.policy.Interval, 30*time.Second))
	defer cancel()
	base := recorder.base
	if base.history != nil {
		if runs, err := base.history.ListRecentRuns(sweepCtx, model.RunListFilter{Terminal: true, Limit: 100}); err == nil {
			for _, run := range runs {
				recorder.observeRun(sweepCtx, run)
			}
		}
		if active, err := base.history.ListActiveRuns(sweepCtx, model.RunListFilter{}); err == nil {
			recorder.observeActive(sweepCtx, active)
		}
	}
	if base.promotions != nil {
		if page, err := base.promotions.ListPromotions(sweepCtx, model.PromotionListFilter{Limit: 50}); err == nil {
			for _, promotion := range page.Promotions {
				recorder.observePromotion(sweepCtx, promotion)
			}
		}
		if prunes, err := recorder.store.PendingPromotionPrunes(sweepCtx, 20); err == nil {
			for _, request := range prunes {
				if promotion, err := base.promotions.Promotion(sweepCtx, request.PromotionID); err == nil {
					recorder.observePromotion(sweepCtx, promotion)
				}
			}
		}
		if publications, err := base.promotions.PendingPublications(sweepCtx); err == nil {
			for _, publication := range publications {
				recorder.observePublicationRetry(sweepCtx, publication)
			}
		}
	}
}

// emit records one event and wakes push streams and run_wait observers.
func (recorder *agentEventRecorder) emit(ctx context.Context, event model.AgentEvent) {
	if recorder.memo[event.Key] {
		return
	}
	if event.Repo == "" && event.RepoID != 0 {
		event.Repo, _ = recorder.base.repositoryIdentity(ctx, event.RepoID)
	}
	_, inserted, err := recorder.store.AppendAgentEvent(ctx, event, recorder.policy.Retention)
	if err != nil {
		log.Printf("WARN: agent event %s not recorded", event.Kind)
		return
	}
	if len(recorder.memo) >= agentEventMemoLimit {
		recorder.memo = map[string]bool{}
	}
	recorder.memo[event.Key] = true
	if !inserted {
		return
	}
	recorder.broadcast()
	if event.RunID != "" && event.Kind != model.AgentEventRunFinished {
		// run_wait observers wake on run signals; the recorder's own
		// observer sees a key it already recorded and stops there.
		recorder.base.signals.NotifyRun(event.RunID)
	}
}

func boundedText(text string, limit int) string {
	text = strings.Join(strings.Fields(text), " ")
	if runes := []rune(text); len(runes) > limit {
		return string(runes[:limit-1]) + "…"
	}
	return text
}

func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

func (recorder *agentEventRecorder) observeRun(ctx context.Context, run model.Run) {
	recorder.observeAbandons(ctx, run)
	if !run.Status.Terminal() || run.FinishedAt == nil || run.FinishedAt.Before(recorder.started.Add(-agentEventLookback)) {
		return
	}
	key := model.AgentEventRunFinished + ":" + run.ID
	if recorder.memo[key] {
		return
	}
	shown := recorder.base.presentationRun(ctx, run)
	hint := runRetryHint(shown, nil)
	data := map[string]any{"status": string(run.Status), "display_status": shown.DisplayStatus, "display_reason": shown.DisplayReason,
		"phase": run.Phase, "actor": run.Actor, "trigger": run.Trigger, "release": run.Release, "finished_at": run.FinishedAt.UTC().Format(time.RFC3339)}
	if run.FailedStep != "" {
		data["failed_step"] = run.FailedStep
	}
	if run.FailedStage != "" {
		data["failed_burn"] = run.FailedStage
	}
	if run.Error != "" {
		data["error"] = boundedText(run.Error, agentEventDataErrorLength)
	}
	if hint != nil {
		data["retry_hint"] = map[string]any{"class": hint.Class, "cause": hint.Cause, "action": hint.Action}
	}
	if run.Assisted {
		data["assisted"] = true
	}
	summary := fmt.Sprintf("run %s %s %s@%s %s", shortRunID(run.ID), "%s", run.Ref, shortSHA(run.SHA), run.Status)
	if shown.DisplayReason != "" && run.Status != model.RunPassed {
		summary += ": " + boundedText(shown.DisplayReason, agentEventSummaryErrorLength)
	}
	if hint != nil {
		summary += " (" + hint.Class + ")"
	}
	event := model.AgentEvent{Kind: model.AgentEventRunFinished, Key: key, RepoID: run.RepoID, RunID: run.ID, Ref: run.Ref, SHA: run.SHA, Data: data}
	event.Repo, _ = recorder.base.repositoryIdentity(ctx, run.RepoID)
	event.Summary = fmt.Sprintf(summary, event.Repo)
	recorder.emit(ctx, event)
	if run.Release && run.Status == model.RunPassed && !run.Assisted {
		recorder.emit(ctx, model.AgentEvent{Kind: model.AgentEventReleasePublished, Key: model.AgentEventReleasePublished + ":" + run.ID,
			RepoID: run.RepoID, Repo: event.Repo, RunID: run.ID, Ref: run.Ref, SHA: run.SHA,
			Summary: fmt.Sprintf("release %s %s@%s published (run %s)", event.Repo, run.Ref, shortSHA(run.SHA), shortRunID(run.ID)),
			Data:    map[string]any{"tag": run.Ref, "sha": run.SHA, "actor": run.Actor}})
	}
	if run.Status == model.RunFailed && (pushretry.PublicationFailure(run.Error) || run.Phase == "publishing") {
		recorder.emit(ctx, model.AgentEvent{Kind: model.AgentEventPublicationFailed, Key: model.AgentEventPublicationFailed + ":run:" + run.ID + ":final",
			RepoID: run.RepoID, Repo: event.Repo, RunID: run.ID, Ref: run.Ref, SHA: run.SHA,
			Summary: fmt.Sprintf("publication of %s %s@%s failed: %s", event.Repo, run.Ref, shortSHA(run.SHA), boundedText(run.Error, agentEventSummaryErrorLength)),
			Data: map[string]any{"final": true, "error": boundedText(run.Error, agentEventDataErrorLength), "actor": run.Actor,
				"transient": pushretry.Transient(fmt.Errorf("%s", run.Error)) || pushretry.PublicationFailure(run.Error)}})
	}
}

func (recorder *agentEventRecorder) observeActive(ctx context.Context, active []model.Run) {
	observer, _ := recorder.base.schedulingObserver.(SynchronizationObserver)
	observed, activeIDs := map[string]bool{}, map[string]bool{}
	var participants []model.SyncParticipant
	unobserved, budget := 0, agentEventMaxObservedRuns
	for _, run := range active {
		activeIDs[run.ID] = true
		if run.Status != model.RunRunning || run.WorkflowName == "" {
			continue
		}
		if run.AssistRequested {
			recorder.observeHolds(ctx, run)
			recorder.observeAbandons(ctx, run)
		}
		if observer == nil {
			continue
		}
		if budget == 0 {
			unobserved++
			continue
		}
		budget--
		readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		observation, err := observer.ObserveSynchronization(readCtx, run)
		cancel()
		if err != nil || observation == nil {
			unobserved++
			continue
		}
		observed[run.ID] = true
		repo, _ := recorder.base.repositoryIdentity(ctx, run.RepoID)
		for _, key := range observation.Holding {
			participants = append(participants, model.SyncParticipant{Lock: model.SyncLockKey(key), Role: model.SyncHolder, RunID: run.ID, RepoID: run.RepoID, Repo: repo, Workflow: run.WorkflowName})
		}
		for _, key := range observation.Waiting {
			participants = append(participants, model.SyncParticipant{Lock: model.SyncLockKey(key), Role: model.SyncWaiter, RunID: run.ID, RepoID: run.RepoID, Repo: repo, Workflow: run.WorkflowName})
		}
	}
	if observer == nil {
		return
	}
	now := recorder.now().UTC()
	if err := recorder.store.RecordSyncObservation(ctx, now, participants, observed, activeIDs, recorder.policy.SyncRetention); err != nil {
		log.Print("WARN: native lock observation not recorded")
		return
	}
	recorder.snapshotMu.Lock()
	recorder.snapshot = syncSnapshot{ObservedAt: now, Participants: participants, Unobserved: unobserved}
	recorder.snapshotMu.Unlock()
	recorder.observeLockWaits(ctx, participants, now)
}

// observeLockWaits records run.lock_wait once per wait interval that has
// lasted longer than the policy threshold, naming the lock's holders.
func (recorder *agentEventRecorder) observeLockWaits(ctx context.Context, participants []model.SyncParticipant, now time.Time) {
	holders := map[string][]string{}
	waiting := map[string]model.SyncParticipant{}
	for _, participant := range participants {
		if participant.Role == model.SyncHolder {
			holders[participant.Lock] = append(holders[participant.Lock], participant.RunID)
		} else {
			waiting[participant.Lock+"\x00"+participant.RunID] = participant
		}
	}
	if len(waiting) == 0 {
		return
	}
	intervals, err := recorder.store.SyncIntervals(ctx, model.SyncIntervalFilter{Since: now, Limit: 2000})
	if err != nil {
		return
	}
	for _, interval := range intervals {
		participant, ok := waiting[interval.Lock+"\x00"+interval.RunID]
		if !ok || interval.Role != model.SyncWaiter || interval.EndedAt != nil {
			continue
		}
		waited := now.Sub(interval.StartedAt)
		if waited < recorder.policy.LockWait {
			continue
		}
		lockHolders := holders[interval.Lock]
		if lockHolders == nil {
			lockHolders = []string{}
		}
		short := make([]string, len(lockHolders))
		for index, id := range lockHolders {
			short[index] = shortRunID(id)
		}
		run := model.Run{ID: interval.RunID}
		if recorder.base.history != nil {
			run, _ = recorder.base.history.Run(ctx, interval.RunID)
		}
		recorder.emit(ctx, model.AgentEvent{Kind: model.AgentEventRunLockWait,
			Key:    fmt.Sprintf("%s:%s:%s:%d", model.AgentEventRunLockWait, interval.RunID, interval.Lock, interval.StartedAt.UnixNano()),
			RepoID: participant.RepoID, Repo: participant.Repo, RunID: interval.RunID, Ref: run.Ref, SHA: run.SHA,
			Summary: fmt.Sprintf("run %s %s %s waits %s for lock %s held by %s", shortRunID(interval.RunID), participant.Repo, run.Ref,
				waited.Round(time.Second), interval.Lock, holderText(short)),
			Data: map[string]any{"lock": interval.Lock, "waiting_since": interval.StartedAt.UTC().Format(time.RFC3339), "waited_s": int64(waited / time.Second),
				"holders": lockHolders, "holder_count": len(lockHolders), "actor": run.Actor}})
	}
}

func holderText(holders []string) string {
	if len(holders) == 0 {
		return "no observed run (another namespace or a run Oberth does not own)"
	}
	return strings.Join(holders, ", ")
}

// observeHolds records run.held once per parked Pod: step, pod, held_since
// and abandon_at (held_since + assist.holdTTL), and how many steps the run
// holds, so an agent services the right one before the TTL.
func (recorder *agentEventRecorder) observeHolds(ctx context.Context, run model.Run) {
	operator := recorder.base.assistHolds
	if operator == nil || !operator.AssistPolicy().Enabled {
		return
	}
	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	holds, err := operator.ListAssistHolds(readCtx, run.WorkflowName)
	cancel()
	if err != nil {
		return
	}
	var parked []model.AssistHold
	for _, hold := range holds {
		if !assistReleased(run.Assist, hold.Pod) {
			parked = append(parked, hold)
		}
	}
	if len(parked) == 0 {
		return
	}
	ttl := operator.AssistPolicy().HoldTTL
	steps := make([]string, len(parked))
	for index, hold := range parked {
		steps[index] = hold.Step
	}
	repo, _ := recorder.base.repositoryIdentity(ctx, run.RepoID)
	for _, hold := range parked {
		abandonAt := hold.HeldSince.Add(ttl)
		summary := fmt.Sprintf("run %s %s %s@%s holds step %s since %s; abandoned at %s unless run_continue or run_hold_abandon", shortRunID(run.ID), repo, run.Ref, shortSHA(run.SHA),
			hold.Step, hold.HeldSince.UTC().Format(time.RFC3339), abandonAt.UTC().Format(time.RFC3339))
		if len(parked) > 1 {
			summary += fmt.Sprintf(" (%d steps held: name step)", len(parked))
		}
		recorder.emit(ctx, model.AgentEvent{Kind: model.AgentEventRunHeld, Key: model.AgentEventRunHeld + ":" + run.ID + ":" + hold.Pod,
			RepoID: run.RepoID, Repo: repo, RunID: run.ID, Ref: run.Ref, SHA: run.SHA, Summary: summary,
			Data: map[string]any{"step": hold.Step, "pod": hold.Pod, "attempt": hold.Attempt, "held_since": hold.HeldSince.UTC().Format(time.RFC3339),
				"abandon_at": abandonAt.UTC().Format(time.RFC3339), "hold_ttl_s": int64(ttl / time.Second), "holds_in_run": len(parked),
				"multiple_holds": len(parked) > 1, "held_steps": steps, "patch_capable": hold.PatchCapable, "actor": run.Actor}})
	}
}

func (recorder *agentEventRecorder) observePublicationRetry(ctx context.Context, publication model.Publication) {
	if publication.Attempts == 0 || !strings.HasPrefix(publication.Error, publicationRetryPrefix) {
		return
	}
	data := map[string]any{"final": false, "publication_id": publication.ID, "attempt": publication.Attempts, "max_attempts": pushretry.MaxAttempts,
		"error": boundedText(publication.Error, agentEventDataErrorLength), "actor": publication.Actor}
	if !publication.RetryAfter.IsZero() {
		data["next_retry_at"] = publication.RetryAfter.UTC().Format(time.RFC3339)
	}
	if publication.PromotionID != "" {
		data["promotion_id"] = publication.PromotionID
	}
	repo, _ := recorder.base.repositoryIdentity(ctx, publication.RepoID)
	recorder.emit(ctx, model.AgentEvent{Kind: model.AgentEventPublicationFailed, Key: fmt.Sprintf("%s:%s:%d", model.AgentEventPublicationFailed, publication.ID, publication.Attempts),
		RepoID: publication.RepoID, Repo: repo, RunID: publication.RunID, PromotionID: publication.PromotionID, Ref: publication.Ref, SHA: publication.ResultSHA,
		Summary: fmt.Sprintf("publication of %s %s@%s failed (attempt %d of %d); retrying automatically", repo, publication.Ref, shortSHA(publication.ResultSHA),
			publication.Attempts, pushretry.MaxAttempts),
		Data: data})
}

func (recorder *agentEventRecorder) observePromotion(ctx context.Context, promotion model.Promotion) {
	if promotion.Status == model.PromotionPending {
		return
	}
	key := model.AgentEventPromotionFinished + ":" + promotion.ID
	if recorder.memo[key] {
		return
	}
	// A prune_source request is settled before the lookback applies: a
	// request left pending across a restart must still be decided, and its
	// promotion.finished event must still be emitted.
	request, requestErr := recorder.store.PromotionPrune(ctx, promotion.ID)
	if requestErr == nil && request.Status == model.PromotionPrunePending {
		request = recorder.base.settlePromotionPrune(ctx, recorder.store, promotion, request, recorder.now())
		if request.Status == model.PromotionPrunePending {
			return // Retried on the next sweep; the event carries the outcome.
		}
	}
	if requestErr != nil && !promotion.UpdatedAt.IsZero() && promotion.UpdatedAt.Before(recorder.started.Add(-agentEventLookback)) {
		return
	}
	data := map[string]any{"status": string(promotion.Status), "source_branch": promotion.SourceBranch, "source_sha": promotion.SourceSHA,
		"target_ref": promotion.TargetRef, "actor": promotion.Actor}
	if promotion.ResultSHA != "" {
		data["result_sha"] = promotion.ResultSHA
	}
	if promotion.RunID != "" {
		data["run_id"] = promotion.RunID
	}
	if promotion.Error != "" {
		data["error"] = boundedText(promotion.Error, agentEventDataErrorLength)
	}
	summary := fmt.Sprintf("promotion %s %s %s -> %s %s", shortRunID(promotion.ID), "%s", shortSHA(promotion.SourceSHA), promotion.TargetRef, promotion.Status)
	if requestErr == nil {
		data["prune_source"] = map[string]any{"status": request.Status, "reason": request.Reason, "batch_id": request.BatchID}
		summary += "; source " + request.Status
		if request.Reason != "" {
			summary += ": " + boundedText(request.Reason, agentEventSummaryErrorLength)
		}
	}
	if promotion.Status != model.PromotionPassed && promotion.Error != "" {
		summary += ": " + boundedText(promotion.Error, agentEventSummaryErrorLength)
	}
	event := model.AgentEvent{Kind: model.AgentEventPromotionFinished, Key: key, RepoID: promotion.RepoID, RunID: promotion.RunID, PromotionID: promotion.ID,
		Ref: promotion.TargetRef, SHA: promotion.SourceSHA, Data: data}
	event.Repo, _ = recorder.base.repositoryIdentity(ctx, promotion.RepoID)
	event.Summary = fmt.Sprintf(summary, event.Repo)
	recorder.emit(ctx, event)
}

// latestSync returns a copy of the last native lock observation.
func (recorder *agentEventRecorder) latestSync() syncSnapshot {
	recorder.snapshotMu.Lock()
	defer recorder.snapshotMu.Unlock()
	snapshot := recorder.snapshot
	snapshot.Participants = append([]model.SyncParticipant(nil), snapshot.Participants...)
	return snapshot
}

// observeAbandons records run.hold_abandoned once per abandoned held Pod from
// the run's assist record: the server sweep's abandon (reason ttl or cap,
// with held_since and the failed attempt's exit code) or the agent's own.
func (recorder *agentEventRecorder) observeAbandons(ctx context.Context, run model.Run) {
	if run.Assist == nil {
		return
	}
	repo := ""
	for _, event := range run.Assist.Events {
		if event.Kind != model.RunAssistAbandon || event.Pod == "" || event.At.Before(recorder.started.Add(-agentEventLookback)) {
			continue
		}
		key := model.AgentEventRunHoldAbandoned + ":" + run.ID + ":" + event.Pod
		if recorder.memo[key] {
			continue
		}
		if repo == "" {
			repo, _ = recorder.base.repositoryIdentity(ctx, run.RepoID)
		}
		reason := event.AbandonReason
		if reason == "" {
			reason = "agent"
		}
		data := map[string]any{"step": event.Step, "pod": event.Pod, "abandon_reason": reason, "abandoned_by": event.Actor,
			"abandoned_at": event.At.UTC().Format(time.RFC3339), "actor": run.Actor}
		summary := fmt.Sprintf("run %s %s %s@%s hold of step %s abandoned (%s)", shortRunID(run.ID), repo, run.Ref, shortSHA(run.SHA), event.Step, reason)
		if event.HeldSince != nil {
			data["held_since"] = event.HeldSince.UTC().Format(time.RFC3339)
			summary += " after " + event.At.Sub(*event.HeldSince).Round(time.Second).String()
		}
		if event.AbandonReason != "" {
			data["failed_attempt"] = event.FailedAttempt
			if event.FailedExitCode != nil {
				data["failed_exit_code"] = *event.FailedExitCode
				summary += fmt.Sprintf("; attempt %d exited %d", event.FailedAttempt, *event.FailedExitCode)
			}
		}
		recorder.emit(ctx, model.AgentEvent{Kind: model.AgentEventRunHoldAbandoned, Key: key, RepoID: run.RepoID, Repo: repo, RunID: run.ID,
			Ref: run.Ref, SHA: run.SHA, At: event.At, Summary: summary, Data: data})
	}
}
