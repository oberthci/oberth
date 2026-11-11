package service

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

func (f *agentFixture) eventsWait(t *testing.T, actor api.Actor, arguments map[string]any) (EventsWaitResponse, error) {
	t.Helper()
	raw, _ := json.Marshal(arguments)
	value, err := f.service.CallTool(t.Context(), actor, "events_wait", raw)
	response, _ := value.(EventsWaitResponse)
	return response, err
}

func (f *agentFixture) finish(t *testing.T, run model.Run) {
	t.Helper()
	if _, err := f.store.FinishRun(t.Context(), run.ID, model.RunResult{Status: model.RunFailed, Phase: "test", FailedStage: "test", FailedStep: "unit", Error: "exit 1"}); err != nil {
		t.Fatal(err)
	}
	f.recorder.handle(t.Context(), agentNotification{kind: "run", id: run.ID})
}

// events_wait blocks until a matching event is recorded, wakes on it, and
// returns the events_since page; a run filter skips other runs' events.
func TestEventsWaitBlocksUntilAMatchingEvent(t *testing.T) {
	f := newAgentFixture(t)
	other := f.enqueue(t, "land/other", "1", false)
	watched := f.enqueue(t, "land/watched", "2", false)
	done := make(chan EventsWaitResponse, 1)
	failed := make(chan error, 1)
	go func() {
		response, err := f.eventsWait(t, agentActor, map[string]any{"run_ids": []string{watched.ID[:12]}, "timeout_s": 2})
		if err != nil {
			failed <- err
			return
		}
		done <- response
	}()
	time.Sleep(50 * time.Millisecond)
	f.finish(t, other)
	select {
	case response := <-done:
		t.Fatalf("returned for another run's event: %+v", response)
	case err := <-failed:
		t.Fatal(err)
	case <-time.After(100 * time.Millisecond):
	}
	f.finish(t, watched)
	var response EventsWaitResponse
	select {
	case response = <-done:
	case err := <-failed:
		t.Fatal(err)
	case <-time.After(testTimeout(5 * time.Second)):
		t.Fatal("events_wait did not wake on the matching event")
	}
	if response.TimedOut || len(response.Events) != 1 || response.Events[0].RunID != watched.ID || response.NextAfterID != response.Events[0].ID {
		t.Fatalf("response = %+v", response)
	}
	// With the cursor of the first event, the next matching one is returned
	// at once from the log, without waiting.
	first := f.events(t, agentActor, map[string]any{}).Events[0]
	response, err := f.eventsWait(t, agentActor, map[string]any{"after_id": first.ID - 1, "kinds": []string{model.AgentEventRunFinished}, "timeout_s": 1})
	if err != nil || len(response.Events) == 0 || response.Events[0].ID != first.ID {
		t.Fatalf("from cursor = %+v, %v", response, err)
	}
}

func TestEventsWaitTimesOutWithAMovedCursor(t *testing.T) {
	f := newAgentFixture(t)
	run := f.enqueue(t, "land/a", "3", false)
	f.finish(t, run)
	latest := f.events(t, agentActor, map[string]any{}).LatestID
	response, err := f.eventsWait(t, agentActor, map[string]any{"after_id": 0, "kinds": []string{model.AgentEventRunHeld}, "timeout_s": 1})
	if err != nil || !response.TimedOut || len(response.Events) != 0 || response.NextAfterID != latest {
		t.Fatalf("timeout = %+v, %v", response, err)
	}
}

func TestEventsWaitRefusals(t *testing.T) {
	f := newAgentFixture(t)
	for name, arguments := range map[string]map[string]any{
		"removed timeout alias": {"timeout": 1},
		"removed seconds alias": {"timeout_seconds": 1},
		"over the maximum":      {"timeout_s": 601},
		"bad kind":              {"kinds": []string{"run.bogus"}},
		"negative cursor":       {"after_id": -1},
		"unknown argument":      {"since": 1},
	} {
		if _, err := f.eventsWait(t, agentActor, arguments); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s = %v", name, err)
		}
	}
	if _, err := f.eventsWait(t, agentActor, map[string]any{"run_ids": []string{strings.Repeat("e", 32)}}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing run = %v", err)
	}
}

// Every long poll takes timeout_s; the removed timeout and timeout_seconds
// spellings are refused as unknown arguments.
func TestLongPollsTakeOnlyTimeoutS(t *testing.T) {
	f := newAgentFixture(t)
	run := f.enqueue(t, "land/alias", "4", false)
	raw, _ := json.Marshal(map[string]any{"id": run.ID, "timeout_s": 1})
	value, err := f.service.CallTool(t.Context(), agentActor, "run_wait", raw)
	if response, _ := value.(RunWaitResponse); err != nil || response.State != runWaitTimeout {
		t.Fatalf("run_wait timeout_s = %+v, %v", value, err)
	}
	if _, err := f.service.CallTool(t.Context(), agentActor, "wait", raw); err != nil {
		t.Fatalf("wait timeout_s = %v", err)
	}
	for _, tool := range []string{"run_wait", "wait"} {
		for _, removed := range []string{"timeout", "timeout_seconds"} {
			raw, _ := json.Marshal(map[string]any{"id": run.ID, removed: 1})
			if _, err := f.service.CallTool(t.Context(), agentActor, tool, raw); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("%s accepted removed %s: %v", tool, removed, err)
			}
		}
	}
}

// The agent loop's one call: events_wait with the kinds run.held and
// run.finished, own and the cursor of the previous page. It returns the
// caller's own events after the cursor, skips another uplink's, and with
// next_after_id as the next cursor never returns an event twice.
func TestEventsWaitServesTheAgentLoopCall(t *testing.T) {
	f := newAgentFixture(t)
	loop := func(actor api.Actor, after int64) EventsWaitResponse {
		t.Helper()
		response, err := f.eventsWait(t, actor, map[string]any{
			"kinds": []string{model.AgentEventRunHeld, model.AgentEventRunFinished}, "own": true, "after_id": after, "timeout_s": 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	first := f.enqueue(t, "land/first", "1", false)
	f.finish(t, first)
	page := loop(agentActor, 0)
	if page.TimedOut || len(page.Events) != 1 || page.Events[0].Kind != model.AgentEventRunFinished || page.Events[0].RunID != first.ID {
		t.Fatalf("first page = %+v", page)
	}
	cursor := page.NextAfterID
	if cursor != page.Events[0].ID {
		t.Fatalf("next_after_id = %d, want the event's id %d", cursor, page.Events[0].ID)
	}
	// Nothing new: the wait times out and the cursor does not move back.
	if page = loop(agentActor, cursor); !page.TimedOut || len(page.Events) != 0 || page.NextAfterID < cursor {
		t.Fatalf("idle page = %+v", page)
	}
	second := f.enqueue(t, "land/second", "2", false)
	f.finish(t, second)
	page = loop(agentActor, cursor)
	if len(page.Events) != 1 || page.Events[0].RunID != second.ID || page.Events[0].ID <= cursor {
		t.Fatalf("second page = %+v", page)
	}
	// Another uplink sees none of these runs as its own.
	other := api.Actor{Identity: "other@host", Admin: true}
	if page = loop(other, 0); !page.TimedOut || len(page.Events) != 0 {
		t.Fatalf("another uplink's own events = %+v", page)
	}
}
