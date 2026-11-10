package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

// MCP agent loop tools (#1514): events_subscribe, events_since, run_wait and
// sync_status. Every read is scoped by the caller's uplink scope; nothing
// here mutates a run, a promotion or a repository.

const (
	maxEventSubscriptionRepos = 32
	maxEventGlobBytes         = 255
	defaultEventPage          = 100
	maxEventPage              = 200
	maxSyncHistoryIntervals   = 2000
	defaultSyncHistoryWindow  = 24 * time.Hour
	maxSyncHistoryWindow      = 30 * 24 * time.Hour
	runWaitPoll               = 5 * time.Second
)

type eventFilterArguments struct {
	Repos    []string `json:"repos"`
	RefsGlob string   `json:"refs_glob"`
	Kinds    []string `json:"kinds"`
	Own      bool     `json:"own"`
}

func (service *API) requireAgentEvents() (*agentEventRecorder, error) {
	if service.agentEvents == nil {
		return nil, refuse(ErrUnavailable, "agent events")
	}
	return service.agentEvents, nil
}

// normalizeEventSubscription resolves repositories through the caller's
// scope (a hidden repository answers like a missing one) to their qualified
// names, and validates the glob and kinds.
func (service *API) normalizeEventSubscription(ctx context.Context, arguments eventFilterArguments) (api.EventSubscription, error) {
	subscription := api.EventSubscription{RefsGlob: strings.TrimSpace(arguments.RefsGlob), Own: arguments.Own}
	if len(arguments.Repos) > maxEventSubscriptionRepos {
		return subscription, fmt.Errorf("%w: at most %d repos", ErrInvalidInput, maxEventSubscriptionRepos)
	}
	for index, name := range arguments.Repos {
		repository, err := service.runs.RepositoryByName(ctx, strings.TrimSpace(name))
		if errors.Is(err, store.ErrNotFound) {
			// Hidden and missing answer alike, without echoing the name.
			return subscription, fmt.Errorf("%w: repos[%d] names no repository", store.ErrNotFound, index)
		}
		if err != nil {
			return subscription, err
		}
		qualified, err := service.repositoryIdentity(ctx, repository.ID)
		if err != nil {
			return subscription, err
		}
		if !contains(subscription.Repos, qualified) {
			subscription.Repos = append(subscription.Repos, qualified)
		}
	}
	if len(subscription.RefsGlob) > maxEventGlobBytes {
		return subscription, fmt.Errorf("%w: refs_glob is at most %d bytes", ErrInvalidInput, maxEventGlobBytes)
	}
	if subscription.RefsGlob != "" {
		if _, err := path.Match(subscription.RefsGlob, ""); err != nil {
			return subscription, fmt.Errorf("%w: refs_glob is not a valid pattern", ErrInvalidInput)
		}
	}
	for _, kind := range arguments.Kinds {
		if !model.ValidAgentEventKind(kind) {
			return subscription, fmt.Errorf("%w: unknown event kind %q (one of %s)", ErrInvalidInput, kind, strings.Join(model.AgentEventKinds(), ", "))
		}
		if !contains(subscription.Kinds, kind) {
			subscription.Kinds = append(subscription.Kinds, kind)
		}
	}
	return subscription, nil
}

func contains(values []string, value string) bool {
	for _, existing := range values {
		if existing == value {
			return true
		}
	}
	return false
}

func (service *API) eventsSubscribe(ctx context.Context, raw json.RawMessage) (api.EventsSubscribeResponse, error) {
	recorder, err := service.requireAgentEvents()
	if err != nil {
		return api.EventsSubscribeResponse{}, err
	}
	var arguments eventFilterArguments
	if err := decodeTool(raw, &arguments); err != nil {
		return api.EventsSubscribeResponse{}, err
	}
	subscription, err := service.normalizeEventSubscription(ctx, arguments)
	if err != nil {
		return api.EventsSubscribeResponse{}, err
	}
	_, latest, err := recorder.store.AgentEventBounds(ctx)
	if err != nil {
		return api.EventsSubscribeResponse{}, err
	}
	return api.EventsSubscribeResponse{Subscription: subscription, LatestID: latest, Push: "none: no MCP session; poll events_since"}, nil
}

func (service *API) eventsSince(ctx context.Context, actor api.Actor, raw json.RawMessage) (api.EventPage, error) {
	var arguments struct {
		eventFilterArguments
		AfterID int64 `json:"after_id"`
		Limit   int   `json:"limit"`
	}
	if err := decodeTool(raw, &arguments); err != nil {
		return api.EventPage{}, err
	}
	if arguments.AfterID < 0 {
		return api.EventPage{}, fmt.Errorf("%w: after_id must not be negative", ErrInvalidInput)
	}
	if arguments.Limit < 0 || arguments.Limit > maxEventPage {
		return api.EventPage{}, fmt.Errorf("%w: limit is 1..%d", ErrInvalidInput, maxEventPage)
	}
	subscription, err := service.normalizeEventSubscription(ctx, arguments.eventFilterArguments)
	if err != nil {
		return api.EventPage{}, err
	}
	return service.AgentEventsFor(ctx, actor, subscription, arguments.AfterID, arguments.Limit)
}

// AgentEventWake is closed and replaced whenever a new event is recorded.
func (service *API) AgentEventWake() <-chan struct{} {
	if service.agentEvents == nil {
		return nil
	}
	return service.agentEvents.wakeChannel()
}

// AgentEventsFor returns events after the cursor that the actor's scope and
// the (already normalized) subscription admit.
func (service *API) AgentEventsFor(ctx context.Context, actor api.Actor, subscription api.EventSubscription, after int64, limit int) (api.EventPage, error) {
	recorder, err := service.requireAgentEvents()
	if err != nil {
		return api.EventPage{}, err
	}
	scoped, err := service.forActor(ctx, actor)
	if err != nil {
		return api.EventPage{}, err
	}
	if limit <= 0 || limit > maxEventPage {
		limit = defaultEventPage
	}
	filter := model.AgentEventFilter{AfterID: after, Kinds: subscription.Kinds, Limit: limit}
	if scoped.scope != nil {
		filter.RepoIDs = scoped.scope.repoIDs
	}
	oldest, latest, err := recorder.store.AgentEventBounds(ctx)
	if err != nil {
		return api.EventPage{}, err
	}
	raw, err := recorder.store.AgentEvents(ctx, filter)
	if err != nil {
		return api.EventPage{}, err
	}
	page := api.EventPage{Events: []model.AgentEvent{}, NextAfterID: max(after, 0), LatestID: latest, OldestID: oldest, HasMore: len(raw) == limit,
		Gap: after > 0 && oldest > 0 && after < oldest-1}
	for _, event := range raw {
		page.NextAfterID = event.ID
		if subscription.Matches(event, actor.Identity) {
			page.Events = append(page.Events, service.scopeLockWaitHolders(ctx, scoped.scope, event))
		}
	}
	if !page.HasMore && page.NextAfterID < latest && len(raw) < limit {
		// Every newer event is outside the scope or the kinds filter.
		page.NextAfterID = latest
	}
	return page, nil
}

// scopeLockWaitHolders keeps a run.lock_wait event's holders inside the
// reader's scope. The recorder stores every holder of the lock, which may be
// runs of repositories the reader cannot see; for a scoped reader those are
// dropped from holders and the summary and counted in hidden_holders
// (holder_count stays the total). A holder whose run cannot be read counts
// as hidden. Unscoped readers get the event unchanged.
func (service *API) scopeLockWaitHolders(ctx context.Context, scope *repoScope, event model.AgentEvent) model.AgentEvent {
	if scope == nil || event.Kind != model.AgentEventRunLockWait {
		return event
	}
	raw, ok := event.Data["holders"].([]any)
	if !ok {
		return event
	}
	visible, short, hidden := []string{}, []string{}, 0
	for _, value := range raw {
		id, _ := value.(string)
		if id == "" || service.history == nil {
			hidden++
			continue
		}
		run, err := service.history.Run(ctx, id)
		if err != nil || !scope.seesRepo(run.RepoID) {
			hidden++
			continue
		}
		visible = append(visible, id)
		short = append(short, shortRunID(id))
	}
	if hidden == 0 {
		return event
	}
	data := make(map[string]any, len(event.Data)+1)
	for key, value := range event.Data {
		data[key] = value
	}
	data["holders"], data["hidden_holders"] = visible, hidden
	event.Data = data
	if index := strings.LastIndex(event.Summary, " held by "); index >= 0 {
		text := fmt.Sprintf("%d run(s) outside your scope", hidden)
		if len(short) > 0 {
			text = strings.Join(short, ", ") + fmt.Sprintf(" and %d run(s) outside your scope", hidden)
		}
		event.Summary = event.Summary[:index] + " held by " + text
	}
	return event
}

// RunWaitResponse is run_wait: why the wait returned and the run then.
type RunWaitResponse struct {
	State     string             `json:"state"`
	Run       RunSummaryResponse `json:"run"`
	Holds     []model.AssistHold `json:"holds"`
	RetryHint *model.RetryHint   `json:"retry_hint,omitempty"`
	WaitedS   int64              `json:"waited_s"`
}

// run_wait states.
const (
	runWaitFinished = "finished"
	runWaitHeld     = "held"
	runWaitChanged  = "changed"
	runWaitTimeout  = "timeout"
)

type runWaitObservation struct {
	run   model.Run
	holds []model.AssistHold
	sig   string
}

func (service *API) prepareRunWait(ctx context.Context, raw json.RawMessage) (func(context.Context) (RunWaitResponse, error), error) {
	var arguments struct {
		ID       string `json:"id"`
		Until    string `json:"until"`
		TimeoutS int    `json:"timeout_s"`
	}
	if err := decodeTool(raw, &arguments); err != nil {
		return nil, err
	}
	if strings.TrimSpace(arguments.ID) == "" {
		return nil, fmt.Errorf("%w: run ID is required", ErrInvalidInput)
	}
	until := arguments.Until
	if until == "" {
		until = runWaitFinished
	}
	if until != runWaitFinished && until != runWaitHeld && until != "any" {
		return nil, fmt.Errorf("%w: until is finished, held or any", ErrInvalidInput)
	}
	if service.history == nil {
		return nil, refuse(ErrUnavailable, "run history")
	}
	duration, err := service.waitDuration(arguments.TimeoutS)
	if err != nil {
		return nil, err
	}
	run, err := service.resolveReadRunID(ctx, arguments.ID)
	if err != nil {
		return nil, err
	}
	clock := service.schedulingClock()
	started := clock.Now()
	deadline := started.Add(duration)
	observe := func(ctx context.Context) (runWaitObservation, error) {
		current, err := service.history.Run(ctx, run.ID)
		if err != nil {
			return runWaitObservation{}, err
		}
		observation := runWaitObservation{run: current, holds: []model.AssistHold{}}
		if current.AssistRequested && current.Status == model.RunRunning && current.WorkflowName != "" && service.assistHolds != nil && service.assistHolds.AssistPolicy().Enabled {
			if holds, err := service.assistHolds.ListAssistHolds(ctx, current.WorkflowName); err == nil {
				activities, ttl := assistActivities(current.Assist), service.assistHolds.AssistPolicy().HoldTTL
				for _, hold := range holds {
					if !assistReleased(current.Assist, hold.Pod) {
						withAbandonAt(&hold, activities, ttl)
						observation.holds = append(observation.holds, hold)
					}
				}
			}
		}
		pods := make([]string, len(observation.holds))
		for index, hold := range observation.holds {
			pods[index] = hold.Pod
		}
		assistEvents := 0
		if current.Assist != nil {
			assistEvents = len(current.Assist.Events)
		}
		observation.sig = fmt.Sprintf("%s|%s|%s|%s|%d|%s", current.Status, current.Phase, current.FailedStep, current.Error, assistEvents, strings.Join(pods, ","))
		return observation, nil
	}
	first, err := observe(ctx)
	if err != nil {
		return nil, err
	}
	respond := func(ctx context.Context, state string, observation runWaitObservation) (RunWaitResponse, error) {
		shown := service.presentationRun(ctx, observation.run)
		summary := observation.run
		summary.Repo, _ = service.repositoryIdentity(ctx, summary.RepoID)
		summary.DisplayStatus, summary.DisplayReason = shown.DisplayStatus, shown.DisplayReason
		steps, _ := service.stepsForRun(ctx, observation.run)
		return RunWaitResponse{State: state, Run: summarizeRun(summary, steps), Holds: observation.holds,
			RetryHint: runRetryHint(observation.run, service.runPublicationRetry(ctx, observation.run)),
			WaitedS:   int64(clock.Since(started) / time.Second)}, nil
	}
	return func(ctx context.Context) (RunWaitResponse, error) {
		current := first
		poll := clock.NewTimer(runWaitPoll)
		defer func() { poll.Stop() }()
		timer := clock.NewTimer(deadline.Sub(clock.Now()))
		defer timer.Stop()
		for {
			switch {
			case current.run.Status.Terminal():
				return respond(ctx, runWaitFinished, current)
			case until != runWaitFinished && len(current.holds) > 0:
				return respond(ctx, runWaitHeld, current)
			case until == "any" && current.sig != first.sig:
				return respond(ctx, runWaitChanged, current)
			}
			changed := service.signals.Run(run.ID)
			select {
			case <-ctx.Done():
				return RunWaitResponse{}, ctx.Err()
			case <-timer.C():
				return respond(ctx, runWaitTimeout, current)
			case <-changed:
			case <-poll.C():
				poll.Stop()
				poll = clock.NewTimer(runWaitPoll)
			}
			next, err := observe(ctx)
			if err != nil {
				return RunWaitResponse{}, err
			}
			current = next
		}
	}, nil
}

// SyncStatusResponse is sync_status: the current holders and waiters of
// every observed native Argo semaphore and mutex, and the bounded history.
type SyncStatusResponse struct {
	Observing  bool              `json:"observing"`
	ObservedAt *time.Time        `json:"observed_at,omitempty"`
	Unobserved int               `json:"unobserved_runs,omitempty"`
	Locks      []SyncLockView    `json:"locks"`
	History    []SyncLockHistory `json:"history"`
	Truncated  bool              `json:"truncated,omitempty"`
}

// SyncLockView is one lock now.
type SyncLockView struct {
	Lock    string       `json:"lock"`
	Holders []SyncMember `json:"holders"`
	Waiters []SyncMember `json:"waiters"`
}

// SyncMember is one run holding or waiting for a lock.
type SyncMember struct {
	RunID   string    `json:"run_id"`
	Repo    string    `json:"repo,omitempty"`
	Since   time.Time `json:"since,omitzero"`
	Seconds int64     `json:"seconds"`
}

// SyncLockHistory summarizes one lock's stored intervals.
type SyncLockHistory struct {
	Lock                 string               `json:"lock"`
	MaxConcurrentHolders int                  `json:"max_concurrent_holders"`
	MaxConcurrentAt      *time.Time           `json:"max_concurrent_at,omitempty"`
	HolderIntervals      int                  `json:"holder_intervals"`
	Waits                SyncWaitStats        `json:"waits"`
	Intervals            []model.SyncInterval `json:"intervals"`
}

// SyncWaitStats are measured wait durations (interval length, sampled).
type SyncWaitStats struct {
	Count        int   `json:"count"`
	MaxSeconds   int64 `json:"max_seconds"`
	TotalSeconds int64 `json:"total_seconds"`
	P50Seconds   int64 `json:"p50_seconds"`
}

func (service *API) syncStatus(ctx context.Context, raw json.RawMessage) (SyncStatusResponse, error) {
	recorder, err := service.requireAgentEvents()
	if err != nil {
		return SyncStatusResponse{}, err
	}
	var arguments struct {
		Lock  string `json:"lock"`
		Since string `json:"since"`
		Limit int    `json:"limit"`
	}
	if err := decodeTool(raw, &arguments); err != nil {
		return SyncStatusResponse{}, err
	}
	window := defaultSyncHistoryWindow
	if arguments.Since != "" {
		parsed, err := parseAge(arguments.Since)
		if err != nil {
			return SyncStatusResponse{}, fmt.Errorf("%w: since must be a positive duration such as 6h or 7d", ErrInvalidInput)
		}
		window = min(parsed, maxSyncHistoryWindow)
	}
	if arguments.Limit < 0 || arguments.Limit > maxSyncHistoryIntervals {
		return SyncStatusResponse{}, fmt.Errorf("%w: limit is 1..%d", ErrInvalidInput, maxSyncHistoryIntervals)
	}
	lock := model.SyncLockKey(arguments.Lock)
	visible := func(repoID int64) bool { return service.scope == nil || service.scope.seesRepo(repoID) }
	snapshot := recorder.latestSync()
	response := SyncStatusResponse{Observing: !snapshot.ObservedAt.IsZero(), Locks: []SyncLockView{}, History: []SyncLockHistory{}}
	if response.Observing {
		at := snapshot.ObservedAt
		response.ObservedAt = &at
		if service.scope == nil {
			response.Unobserved = snapshot.Unobserved
		}
	}
	filter := model.SyncIntervalFilter{Lock: lock, Since: recorder.now().Add(-window), Limit: arguments.Limit}
	if filter.Limit == 0 {
		filter.Limit = maxSyncHistoryIntervals
	}
	if service.scope != nil {
		filter.RepoIDs = service.scope.repoIDs
	}
	intervals, err := recorder.store.SyncIntervals(ctx, filter)
	if err != nil {
		return SyncStatusResponse{}, err
	}
	response.Truncated = len(intervals) == filter.Limit
	open := map[string]model.SyncInterval{}
	for _, interval := range intervals {
		if interval.EndedAt == nil {
			open[interval.Lock+"\x00"+interval.Role+"\x00"+interval.RunID] = interval
		}
	}
	now := recorder.now()
	locks := map[string]*SyncLockView{}
	for _, participant := range snapshot.Participants {
		if (lock != "" && participant.Lock != lock) || !visible(participant.RepoID) {
			continue
		}
		view := locks[participant.Lock]
		if view == nil {
			view = &SyncLockView{Lock: participant.Lock, Holders: []SyncMember{}, Waiters: []SyncMember{}}
			locks[participant.Lock] = view
		}
		member := SyncMember{RunID: participant.RunID, Repo: participant.Repo}
		if interval, ok := open[participant.Lock+"\x00"+participant.Role+"\x00"+participant.RunID]; ok {
			member.Since = interval.StartedAt
			member.Seconds = int64(now.Sub(interval.StartedAt) / time.Second)
		}
		if participant.Role == model.SyncHolder {
			view.Holders = append(view.Holders, member)
		} else {
			view.Waiters = append(view.Waiters, member)
		}
	}
	for _, view := range locks {
		response.Locks = append(response.Locks, *view)
	}
	sort.Slice(response.Locks, func(i, j int) bool { return response.Locks[i].Lock < response.Locks[j].Lock })
	response.History = summarizeSyncHistory(intervals, now)
	return response, nil
}

// summarizeSyncHistory groups intervals by lock and measures the maximum
// number of simultaneous holders (a sweep over interval end points; an open
// interval has not ended) and the wait durations.
func summarizeSyncHistory(intervals []model.SyncInterval, now time.Time) []SyncLockHistory {
	byLock := map[string]*SyncLockHistory{}
	waits := map[string][]int64{}
	type point struct {
		at    time.Time
		delta int
	}
	points := map[string][]point{}
	for _, interval := range intervals {
		history := byLock[interval.Lock]
		if history == nil {
			history = &SyncLockHistory{Lock: interval.Lock, Intervals: []model.SyncInterval{}}
			byLock[interval.Lock] = history
		}
		history.Intervals = append(history.Intervals, interval)
		// An open interval is still held: it overlaps everything after it.
		end := now.Add(time.Hour)
		if interval.EndedAt != nil {
			end = *interval.EndedAt
		}
		if interval.Role == model.SyncHolder {
			history.HolderIntervals++
			points[interval.Lock] = append(points[interval.Lock], point{interval.StartedAt, 1}, point{end, -1})
			continue
		}
		if interval.EndedAt == nil {
			end = now
		}
		waits[interval.Lock] = append(waits[interval.Lock], int64(end.Sub(interval.StartedAt)/time.Second))
	}
	result := make([]SyncLockHistory, 0, len(byLock))
	for lock, history := range byLock {
		sorted := points[lock]
		// Ends sort before starts at the same instant: back-to-back holders
		// handing the lock over in one sample do not count as overlap.
		sort.Slice(sorted, func(i, j int) bool {
			if sorted[i].at.Equal(sorted[j].at) {
				return sorted[i].delta < sorted[j].delta
			}
			return sorted[i].at.Before(sorted[j].at)
		})
		current := 0
		for _, p := range sorted {
			current += p.delta
			if current > history.MaxConcurrentHolders {
				history.MaxConcurrentHolders = current
				at := p.at
				history.MaxConcurrentAt = &at
			}
		}
		durations := waits[lock]
		sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
		history.Waits.Count = len(durations)
		for _, seconds := range durations {
			history.Waits.TotalSeconds += seconds
			history.Waits.MaxSeconds = max(history.Waits.MaxSeconds, seconds)
		}
		if len(durations) > 0 {
			history.Waits.P50Seconds = durations[len(durations)/2]
		}
		result = append(result, *history)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Lock < result[j].Lock })
	return result
}

// dispatchTool serves the agent loop tools (#1514) and passes every other
// tool to callTool. None of these tools mutates, so none passes the gate.
func (service *API) dispatchTool(ctx context.Context, actor api.Actor, name string, raw json.RawMessage) (any, error) {
	switch name {
	case "events_subscribe":
		return service.eventsSubscribe(ctx, raw)
	case "events_since":
		return service.eventsSince(ctx, actor, raw)
	case "sync_status":
		return service.syncStatus(ctx, raw)
	case "run_wait":
		prepared, err := service.prepareRunWait(ctx, raw)
		if err != nil {
			return nil, err
		}
		return prepared(ctx)
	}
	return service.callTool(ctx, actor, name, raw)
}

// promoteTool is the promote tool, with prune_source.
func (service *API) promoteTool(ctx context.Context, actor api.Actor, raw json.RawMessage) (any, error) {
	var arguments promotionArguments
	if err := decodeTool(raw, &arguments); err != nil {
		return nil, err
	}
	if (arguments.SHA != "") == (arguments.Sources != nil) || arguments.SkipConflicting && arguments.Sources == nil {
		return nil, refuse(ErrInvalidInput, "provide either sha or sources; skip_conflicting requires sources")
	}
	if arguments.PruneSource {
		if err := service.checkPruneSource(ctx, arguments); err != nil {
			return nil, err
		}
	}
	var promotion model.Promotion
	var err error
	if arguments.Sources != nil {
		promotion, err = service.promoteTrain(ctx, actor, arguments)
	} else {
		promotion, err = service.promote(ctx, actor, arguments.Repo, arguments.SHA, arguments.Branch, requestedPriority(actor, arguments.PriorityRequest))
	}
	if err != nil {
		return nil, err
	}
	response := api.PromoteResponse{ID: promotion.ID}
	if arguments.PruneSource {
		response.PruneSource = service.requestPromotionPrune(ctx, actor, promotion)
	}
	return response, nil
}
