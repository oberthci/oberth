package store

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

// One key is one fact; retention prunes the oldest events by count.
func TestAgentEventsDedupeAndRetention(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	s := testStore(t, &now)
	ctx := t.Context()
	first, inserted, err := s.AppendAgentEvent(ctx, model.AgentEvent{Kind: model.AgentEventRunFinished, Key: "k", RepoID: 7, Summary: "a  b\nc", Data: map[string]any{"x": 1}}, 0)
	if err != nil || !inserted || first.Summary != "a b c" || first.Data["x"] != float64(1) {
		t.Fatalf("first = %+v %v %v", first, inserted, err)
	}
	again, inserted, err := s.AppendAgentEvent(ctx, model.AgentEvent{Kind: model.AgentEventRunFinished, Key: "k", Summary: "other"}, 0)
	if err != nil || inserted || again.ID != first.ID || again.Summary != "a b c" {
		t.Fatalf("duplicate = %+v %v %v", again, inserted, err)
	}
	if _, _, err := s.AppendAgentEvent(ctx, model.AgentEvent{Kind: "run.bogus", Key: "x"}, 0); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown kind = %v", err)
	}
	if _, _, err := s.AppendAgentEvent(ctx, model.AgentEvent{Kind: model.AgentEventRunHeld, Key: "big", Data: map[string]any{"x": string(make([]byte, model.MaximumAgentEventData))}}, 0); !errors.Is(err, ErrInvalid) {
		t.Fatalf("oversized data = %v", err)
	}
	for index := range 200 {
		if _, _, err := s.AppendAgentEvent(ctx, model.AgentEvent{Kind: model.AgentEventRunHeld, Key: fmt.Sprint("held-", index), RepoID: 8}, 100); err != nil {
			t.Fatal(err)
		}
	}
	oldest, newest, err := s.AgentEventBounds(ctx)
	if err != nil || newest < 201 || newest-oldest >= 164 {
		t.Fatalf("bounds = %d..%d, %v", oldest, newest, err)
	}
	scoped, err := s.AgentEvents(ctx, model.AgentEventFilter{RepoIDs: []int64{7}})
	if err != nil || len(scoped) != 0 {
		t.Fatalf("pruned or scoped events = %+v, %v", scoped, err)
	}
	if none, err := s.AgentEvents(ctx, model.AgentEventFilter{RepoIDs: []int64{}}); err != nil || len(none) != 0 {
		t.Fatalf("empty scope = %+v, %v", none, err)
	}
	page, err := s.AgentEvents(ctx, model.AgentEventFilter{AfterID: newest - 3, Kinds: []string{model.AgentEventRunHeld}, Limit: 2})
	if err != nil || len(page) != 2 || page[0].ID != newest-2 {
		t.Fatalf("page = %+v, %v", page, err)
	}
}

// An active run whose Workflow could not be read keeps its open interval;
// an observed run without the lock and a finished run close theirs.
func TestSyncObservationIntervals(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	s := testStore(t, &now)
	ctx := t.Context()
	lock := "Mutex/m"
	all := []model.SyncParticipant{{Lock: lock, Role: model.SyncHolder, RunID: "a"}, {Lock: lock, Role: model.SyncWaiter, RunID: "b"}, {Lock: lock, Role: model.SyncWaiter, RunID: "c"}}
	every := map[string]bool{"a": true, "b": true, "c": true}
	if err := s.RecordSyncObservation(ctx, now, all, every, every, 0); err != nil {
		t.Fatal(err)
	}
	later := now.Add(10 * time.Second)
	// a: active, unreadable; b: observed without the lock; c: no longer active.
	if err := s.RecordSyncObservation(ctx, later, nil, map[string]bool{"b": true}, map[string]bool{"a": true, "b": true}, 0); err != nil {
		t.Fatal(err)
	}
	intervals, err := s.SyncIntervals(ctx, model.SyncIntervalFilter{Lock: lock})
	if err != nil || len(intervals) != 3 {
		t.Fatalf("intervals = %+v, %v", intervals, err)
	}
	ended := map[string]bool{}
	for _, interval := range intervals {
		ended[interval.RunID] = interval.EndedAt != nil
	}
	if ended["a"] || !ended["b"] || !ended["c"] {
		t.Fatalf("ended = %v", ended)
	}
	if err := s.RecordSyncObservation(ctx, later.Add(time.Second), nil, map[string]bool{"a": true}, map[string]bool{"a": true}, 1); err != nil {
		t.Fatal(err)
	}
	if kept, err := s.SyncIntervals(ctx, model.SyncIntervalFilter{}); err != nil || len(kept) != 1 {
		t.Fatalf("retention kept %+v, %v", kept, err)
	}
}

// Endpoint reads stay live after inserts/deletes, including a sparse ID space.
func TestAgentEventBoundsEndpoints(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	check := func(wantOldest, wantNewest int64) {
		t.Helper()
		oldest, newest, err := s.AgentEventBounds(t.Context())
		if err != nil || oldest != wantOldest || newest != wantNewest {
			t.Fatalf("bounds %d..%d, want %d..%d: %v", oldest, newest, wantOldest, wantNewest, err)
		}
	}
	check(0, 0)
	first, _, err := s.AppendAgentEvent(t.Context(), model.AgentEvent{Key: "first", Kind: model.AgentEventRunFinished}, 0)
	if err != nil {
		t.Fatal(err)
	}
	check(first.ID, first.ID)
	if _, err := s.db.ExecContext(t.Context(), `UPDATE agent_events SET id = 1000000 WHERE id = ?`, first.ID); err != nil {
		t.Fatal(err)
	}
	check(1000000, 1000000)
	last, _, err := s.AppendAgentEvent(t.Context(), model.AgentEvent{Key: "last", Kind: model.AgentEventRunFinished}, 0)
	if err != nil {
		t.Fatal(err)
	}
	check(1000000, last.ID)
	if _, err := s.db.ExecContext(t.Context(), `DELETE FROM agent_events WHERE id = 1000000`); err != nil {
		t.Fatal(err)
	}
	check(last.ID, last.ID)
	if _, err := s.db.ExecContext(t.Context(), `DELETE FROM agent_events`); err != nil {
		t.Fatal(err)
	}
	check(0, 0)
}

// Guard the algorithm, without a timing threshold depending on the test host.
// MIN and MAX in the same aggregate produce a full scan instead of two seeks.
func TestAgentEventBoundsSeeksBothEndpoints(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	rows, err := s.reader().QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+agentEventBoundsQuery)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	searches := 0
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(detail, "SEARCH agent_events") {
			searches++
		}
		if strings.Contains(detail, "SCAN agent_events") {
			t.Fatalf("full event-log scan: %s", detail)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if searches != 2 {
		t.Fatalf("expected two endpoint searches, got %d", searches)
	}
}
