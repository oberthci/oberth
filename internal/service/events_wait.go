package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
)

// maxEventsWaitRuns bounds events_wait run_ids.
const maxEventsWaitRuns = 32

// eventsWaitPoll re-reads the log even without a wake, as run_wait does, so
// an event recorded while the waiter was between reads is never missed.
const eventsWaitPoll = 5 * time.Second

// EventsWaitResponse is events_wait: the page events_since would return for
// the first matching events after the cursor, or an empty page with
// timed_out when none arrived within the bound.
type EventsWaitResponse struct {
	api.EventPage
	TimedOut bool  `json:"timed_out,omitempty"`
	WaitedS  int64 `json:"waited_s"`
}

// prepareEventsWait validates events_wait and resolves its run IDs through
// the caller's scope (a hidden run answers like a missing one) before a
// long poll starts. The returned call blocks until at least one event after
// after_id passes every filter, or until timeout_s.
func (service *API) prepareEventsWait(ctx context.Context, actor api.Actor, raw json.RawMessage) (func(context.Context) (EventsWaitResponse, error), error) {
	var arguments struct {
		eventFilterArguments
		AfterID *int64   `json:"after_id"`
		RunIDs  []string `json:"run_ids"`
		Limit   int      `json:"limit"`
		timeoutArguments
	}
	if err := decodeTool(raw, &arguments); err != nil {
		return nil, err
	}
	if _, err := service.requireAgentEvents(); err != nil {
		return nil, err
	}
	if arguments.Limit < 0 || arguments.Limit > maxEventPage {
		return nil, fmt.Errorf("%w: limit is 1..%d (0 or omitted: the default page)", ErrInvalidInput, maxEventPage)
	}
	if len(arguments.RunIDs) > maxEventsWaitRuns {
		return nil, fmt.Errorf("%w: at most %d run_ids", ErrInvalidInput, maxEventsWaitRuns)
	}
	seconds, err := arguments.seconds()
	if err != nil {
		return nil, err
	}
	duration, err := service.waitDuration(seconds)
	if err != nil {
		return nil, err
	}
	subscription, err := service.normalizeEventSubscription(ctx, arguments.eventFilterArguments)
	if err != nil {
		return nil, err
	}
	runs := map[string]bool{}
	for index, id := range arguments.RunIDs {
		if strings.TrimSpace(id) == "" {
			return nil, fmt.Errorf("%w: run_ids[%d] is empty", ErrInvalidInput, index)
		}
		if service.history == nil {
			return nil, refuse(ErrUnavailable, "run history")
		}
		run, err := service.resolveReadRunID(ctx, strings.TrimSpace(id))
		if err != nil {
			return nil, err
		}
		runs[run.ID] = true
	}
	recorder := service.agentEvents
	after := int64(0)
	if arguments.AfterID != nil {
		if *arguments.AfterID < 0 {
			return nil, fmt.Errorf("%w: after_id must not be negative", ErrInvalidInput)
		}
		after = *arguments.AfterID
	} else {
		// Without a cursor the wait is for what happens next, not for the
		// retained history.
		_, latest, err := recorder.store.AgentEventBounds(ctx)
		if err != nil {
			return nil, err
		}
		after = latest
	}
	clock := service.schedulingClock()
	started := clock.Now()
	deadline := started.Add(duration)
	// read pages forward from the cursor until a page matches or the log
	// is exhausted; the cursor advances past every event scanned.
	read := func(ctx context.Context, cursor int64) (api.EventPage, error) {
		for {
			page, err := service.AgentEventsFor(ctx, actor, subscription, cursor, arguments.Limit)
			if err != nil {
				return page, err
			}
			if len(runs) > 0 {
				matched := page.Events[:0]
				for _, event := range page.Events {
					if runs[event.RunID] {
						matched = append(matched, event)
					}
				}
				page.Events = matched
			}
			if len(page.Events) > 0 || !page.HasMore || page.NextAfterID <= cursor {
				return page, nil
			}
			cursor = page.NextAfterID
		}
	}
	return func(ctx context.Context) (EventsWaitResponse, error) {
		cursor := after
		poll := clock.NewTimer(eventsWaitPoll)
		defer func() { poll.Stop() }()
		timer := clock.NewTimer(deadline.Sub(clock.Now()))
		defer timer.Stop()
		for {
			// Take the wake channel before reading, so an event recorded
			// after the read still wakes this waiter.
			wake := recorder.wakeChannel()
			page, err := read(ctx, cursor)
			if err != nil {
				return EventsWaitResponse{}, err
			}
			if len(page.Events) > 0 {
				return EventsWaitResponse{EventPage: page, WaitedS: int64(clock.Since(started) / time.Second)}, nil
			}
			cursor = max(cursor, page.NextAfterID)
			select {
			case <-ctx.Done():
				return EventsWaitResponse{}, ctx.Err()
			case <-timer.C():
				page.Events = []model.AgentEvent{}
				page.NextAfterID = cursor
				return EventsWaitResponse{EventPage: page, TimedOut: true, WaitedS: int64(clock.Since(started) / time.Second)}, nil
			case <-wake:
			case <-poll.C():
				poll.Stop()
				poll = clock.NewTimer(eventsWaitPoll)
			}
		}
	}, nil
}
