package store

import (
	"testing"

	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
)

func receiptOnly(t *testing.T, s *Store, id string) stateport.BatchAcceptance {
	t.Helper()
	repo := atomicRepository(t, s)
	return stateport.BatchAcceptance{ReceiveID: id, Repository: repo.Canonical, Actor: actor.Actor{Identity: "operator", AuthMethod: "cert"}}
}

func TestReceiptOnlyBatchPersistsAndReplaysAfterRestart(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"all-parked", "deletion", "no-pipeline"} {
		t.Run(kind, func(t *testing.T) { checkReceiptOnlyReplay(t, kind) })
	}
}

func checkReceiptOnlyReplay(t *testing.T, kind string) {
	t.Helper()

	s := testStore(t)
	input := receiptOnly(t, s, kind)
	input.Events = []stateport.EventWrite{atomicEvent(s, kind)}
	before := independentHead(t, s.readers)
	recorded, err := s.AcceptBatch(t.Context(), input)
	runs, fresh := recorded.Runs, recorded.Fresh
	requireOK(t, err)
	if !fresh || len(runs) != 0 {
		t.Fatal("receipt-only push was not recorded", runs, fresh)
	}
	head := independentHead(t, s.readers)
	if head.ID != before.ID+1 {
		t.Fatal("receipt attribution was not audited")
	}
	path, options := s.path, s.options
	requireOK(t, s.Close())
	reopened, err := Open(t.Context(), path, options)
	requireOK(t, err)
	defer func() { requireOK(t, reopened.Close()) }()
	runs, err = reopened.AcceptedRuns(t.Context(), input.ReceiveID)
	requireOK(t, err)
	if len(runs) != 0 {
		t.Fatal("receipt-only lookup fabricated work", runs)
	}
	replayed, err := reopened.AcceptBatch(t.Context(), input)
	runs, fresh = replayed.Runs, replayed.Fresh
	requireOK(t, err)
	if fresh || len(runs) != 0 || independentHead(t, reopened.readers) != head {
		t.Fatal("receipt-only replay repeated work or audit")
	}
	page, err := reopened.Events(t.Context(), stateport.EventFilter{Limit: 200})
	requireOK(t, err)
	if len(page.Events) != 1 {
		t.Fatal("receipt-only replay duplicated its event", page)
	}
	_, _ = accepted(t, reopened, "after-"+kind)
}

func TestReceiptOnlyBatchRefusesChangedContext(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	input := receiptOnly(t, s, "receipt")
	_, err := s.AcceptBatch(t.Context(), input)
	requireOK(t, err)
	input.Actor.Identity = "other"
	_, err = s.AcceptBatch(t.Context(), input)
	requireCode(t, err, publicerr.Conflict)
	input.ReceiveID = "missing-repository"
	input.Repository = "origin/team/missing"
	_, err = s.AcceptBatch(t.Context(), input)
	requireCode(t, err, publicerr.NotFound)
}

func TestReceiptOnlyEventFailureRollsBackReceiptAndAudit(t *testing.T) {
	t.Parallel()
	checkAtomicFacts(t, []atomicFact{{"receive.receipt-only", func(t *testing.T, s *Store) func(stateport.Action) error {
		input := receiptOnly(t, s, "receipt")
		return func(a stateport.Action) error {
			input.Events = a.Events
			_, err := s.AcceptBatch(t.Context(), input)
			return err
		}
	}}})
}
