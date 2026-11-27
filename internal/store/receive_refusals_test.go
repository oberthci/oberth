package store

import (
	"encoding/json"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestReceiveRefusalFloodIsBoundedAndCoalesced(t *testing.T) {
	s := testStore(t)
	now := s.options.Clock()
	s.options.Clock = func() time.Time { return now }
	write := func(actor, repo string, n int) {
		for i := range n {
			v := ReceiveRefusal{Code: "grant", Updates: []ReceiveRefusalRef{{Ref: "refs/heads/probe-" + strconv.Itoa(i)}}}
			requireOK(t, s.RecordReceiveRefusal(t.Context(), actor, repo, v))
		}
	}
	write("agent", "origin/team/widget", 1000)
	actions, _, err := s.Audit(t.Context(), 0, 200)
	requireOK(t, err)
	if len(actions) != 8 {
		t.Fatal("flood exceeded detailed record budget", len(actions))
	}
	rows, err := s.AdministrativeAudit(t.Context(), []string{"origin/team/other"}, stateport.AdministrativeFilter{Limit: 200})
	requireOK(t, err)
	if len(rows) != 0 {
		t.Fatal("another scope saw refusals", rows)
	}
	rows, err = s.AdministrativeAudit(t.Context(), nil, stateport.AdministrativeFilter{Limit: 200})
	requireOK(t, err)
	if len(rows) != 9 || rows[0].Action != "receive.refused.more" || rows[0].Details != `{"more_refusals":992}` {
		t.Fatal("coalesced count is missing", rows)
	}
	rows, err = s.AdministrativeAudit(t.Context(), []string{"origin/team/widget"}, stateport.AdministrativeFilter{Limit: 200})
	requireOK(t, err)
	if len(rows) != 9 {
		t.Fatal("own scope hid detailed or coalesced refusals", rows)
	}
	write("agent", "origin/team/widget", 1000)
	for range 20 {
		rows, err = s.AdministrativeAudit(t.Context(), nil, stateport.AdministrativeFilter{Limit: 200})
		requireOK(t, err)
		if len(rows) != 9 {
			t.Fatal("audit reads bypassed rate bound", len(rows))
		}
	}
	_, _, err = s.Audit(t.Context(), 0, 200)
	requireOK(t, err)
}

func TestReceiveRefusalWindowAndScopeIsolation(t *testing.T) {
	s := testStore(t)
	now := s.options.Clock()
	s.options.Clock = func() time.Time { return now }
	for range 9 {
		requireOK(t, s.RecordReceiveRefusal(t.Context(), "agent", "origin/team/widget", ReceiveRefusal{Code: "grant"}))
	}
	requireOK(t, s.RecordReceiveRefusal(t.Context(), "other-agent", "origin/team/widget", ReceiveRefusal{Code: "grant"}))
	requireOK(t, s.RecordReceiveRefusal(t.Context(), "agent", "origin/team/other", ReceiveRefusal{Code: "grant"}))
	now = now.Add(time.Minute)
	requireOK(t, s.RecordReceiveRefusal(t.Context(), "agent", "origin/team/widget", ReceiveRefusal{Code: "grant"}))
	actions, _, err := s.Audit(t.Context(), 0, 200)
	requireOK(t, err)
	if len(actions) != 12 || actions[10].Details != `{"more_refusals":1}` || actions[11].Action != "receive.refused" {
		t.Fatal("window or actor/repository isolation failed", actions)
	}
}

func TestReceiveRefusalConcurrentCountsAndChain(t *testing.T) {
	s := testStore(t)
	var workers sync.WaitGroup
	for range 64 {
		workers.Go(func() {
			if err := s.RecordReceiveRefusal(t.Context(), "agent", "origin/team/widget", ReceiveRefusal{Code: "grant"}); err != nil {
				t.Error(err)
			}
		})
	}
	workers.Wait()
	rows, err := s.AdministrativeAudit(t.Context(), nil, stateport.AdministrativeFilter{Limit: 200})
	requireOK(t, err)
	if len(rows) != 9 || rows[0].Details != `{"more_refusals":56}` {
		t.Fatal("concurrent refusal count lost", rows)
	}
	_, _, err = s.Audit(t.Context(), 0, 200)
	requireOK(t, err)
}

func TestReceiveRefusalShutdownFlushesAndReopens(t *testing.T) {
	path := t.TempDir() + "/state.sqlite"
	s, err := Create(t.Context(), path, testOptions())
	requireOK(t, err)
	for range 100 {
		requireOK(t, s.RecordReceiveRefusal(t.Context(), "agent", "origin/team/widget", ReceiveRefusal{Code: "grant"}))
	}
	requireOK(t, s.FlushReceiveRefusals(t.Context()))
	requireOK(t, s.Close())
	s, err = Open(t.Context(), path, testOptions())
	requireOK(t, err)
	defer func() { requireOK(t, s.Close()) }()
	actions, head, err := s.Audit(t.Context(), 0, 200)
	requireOK(t, err)
	if head.ID != 9 || actions[8].Details != `{"more_refusals":92}` {
		t.Fatal("close lost pending coalesced count", actions, head)
	}
}

func TestReceiveRefusalWriteFailureDoesNotSpendBudget(t *testing.T) {
	s := testStore(t)
	execute(t, s.writer, `CREATE TRIGGER refusal_failure BEFORE INSERT ON audit BEGIN SELECT RAISE(ABORT,'fixture write failure'); END`)
	if err := s.RecordReceiveRefusal(t.Context(), "agent", "origin/team/widget", ReceiveRefusal{Code: "grant"}); err == nil {
		t.Fatal("failed audit write reported success")
	}
	execute(t, s.writer, `DROP TRIGGER refusal_failure`)
	for range 8 {
		requireOK(t, s.RecordReceiveRefusal(t.Context(), "agent", "origin/team/widget", ReceiveRefusal{Code: "grant"}))
	}
	actions, _, err := s.Audit(t.Context(), 0, 200)
	requireOK(t, err)
	if len(actions) != 8 {
		t.Fatal("write failure consumed budget", actions)
	}
}

func TestReceiveRefusalSummaryFailureKeepsCount(t *testing.T) {
	s := testStore(t)
	for range 20 {
		requireOK(t, s.RecordReceiveRefusal(t.Context(), "agent", "origin/team/widget", ReceiveRefusal{Code: "grant"}))
	}
	execute(t, s.writer, `CREATE TRIGGER refusal_failure BEFORE INSERT ON audit BEGIN SELECT RAISE(ABORT,'fixture write failure'); END`)
	if _, err := s.AdministrativeAudit(t.Context(), nil, stateport.AdministrativeFilter{Limit: 200}); err == nil {
		t.Fatal("summary write failure was hidden")
	}
	execute(t, s.writer, `DROP TRIGGER refusal_failure`)
	rows, err := s.AdministrativeAudit(t.Context(), nil, stateport.AdministrativeFilter{Limit: 200})
	requireOK(t, err)
	if len(rows) != 9 || rows[0].Details != `{"more_refusals":12}` {
		t.Fatal("failed summary lost its count", rows)
	}
}

func TestReceiveRefusalScopeAndPayloadBounds(t *testing.T) {
	s := testStore(t)
	now := s.options.Clock()
	s.refusals.windows = make(map[refusalScope]*refusalWindow)
	for i := range maxReceiveRefusalScopes {
		raw, err := json.Marshal(i)
		requireOK(t, err)
		s.refusals.windows[refusalScope{string(raw), "origin/team/widget"}] = &refusalWindow{start: now}
	}
	requireCode(t, s.RecordReceiveRefusal(t.Context(), "overflow", "origin/team/widget", ReceiveRefusal{Code: "grant"}), publicerr.Unavailable)
	for _, v := range []ReceiveRefusal{{}, {Code: string(make([]byte, 41))}, {Code: "grant", File: string(make([]byte, 384<<10))}} {
		requireCode(t, s.RecordReceiveRefusal(t.Context(), "agent", "origin/team/widget", v), publicerr.InvalidInput)
	}
}
