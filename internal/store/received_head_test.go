package store

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestReceivedHeadsSurviveRestartAndLaterReceives(t *testing.T) {
	s := testStore(t)
	_, input := accepted(t, s, "template")
	q := input.Request.Clone()
	q.ReceiveID, q.PreviousSHA = "actual-push", strings.Repeat("b", 40)
	view := input.Prepared.View()
	view.RunID = "received-run"
	prepared, err := admissioncontract.Seal(view)
	requireOK(t, err)
	batch := stateport.BatchAcceptance{ReceiveID: q.ReceiveID, Repository: q.Repo, Actor: q.Actor, Received: []model.CIRequest{q}, Runs: []stateport.Acceptance{{Request: q, Prepared: prepared}}}
	_, err = s.AcceptBatch(t.Context(), batch)
	requireOK(t, err)
	later := q.Clone()
	later.ReceiveID, later.PreviousSHA = "later-parked-push", strings.Repeat("c", 40)
	_, err = s.AcceptBatch(t.Context(), stateport.BatchAcceptance{ReceiveID: later.ReceiveID, Repository: later.Repo, Actor: later.Actor, Received: []model.CIRequest{later}})
	requireOK(t, err)
	// Replaying an older receipt cannot roll the last-receive index back.
	_, err = s.AcceptBatch(t.Context(), batch)
	requireOK(t, err)
	path, options := s.path, s.options
	requireOK(t, s.Close())
	s, err = Open(t.Context(), path, options)
	requireOK(t, err)
	defer func() { requireOK(t, s.Close()) }()
	for _, tc := range []struct{ run, want string }{{"received-run", q.PreviousSHA}, {"", later.PreviousSHA}} {
		got, err := s.ReceivedHeads(t.Context(), stateport.ReceivedRef{Repo: q.Repo, Ref: q.Ref, ObjectSHA: q.ObjectSHA, RunID: tc.run})
		requireOK(t, err)
		if !slices.Equal(got, []string{tc.want}) {
			t.Fatal("receive evidence changed", tc, got)
		}
	}
	missing, err := s.ReceivedHeads(t.Context(), stateport.ReceivedRef{Repo: q.Repo, Ref: q.Ref, ObjectSHA: strings.Repeat("d", 40)})
	requireOK(t, err)
	if missing != nil {
		t.Fatal("another object inherited receive evidence", missing)
	}
}

func TestPublicationKeepsPrivateReceiveHeadThroughAttemptAndBind(t *testing.T) {
	s := testStore(t)
	r, _ := accepted(t, s, "receive-evidence")
	p := model.Publication{ID: "receive-evidence", Repo: r.Repo, Ref: r.Ref, Actor: r.Origin.Actor, ObjectSHA: r.ObjectSHA, CommitSHA: r.CommitSHA, ReceivedHeads: []string{""}}
	a := stateport.Action{Actor: p.Actor}
	requireOK(t, s.PreparePublication(t.Context(), a, stateport.PublicationIntent{Publication: p}))
	_, err := s.StartPublicationAttempt(t.Context(), a, stateport.PublicationAttempt{ID: p.ID})
	requireOK(t, err)
	got, err := s.BindPublication(t.Context(), a, p.ID, strings.Repeat("b", 40))
	requireOK(t, err)
	if got.ReceivedHeads == nil || got.ReceivedHeads[0] != "" {
		t.Fatal("known-absent receive head was lost", got)
	}
	listed, err := s.Publications(t.Context(), stateport.RecordFilter{Limit: 10})
	requireOK(t, err)
	if len(listed) != 1 || listed[0].ReceivedHeads == nil || listed[0].ReceivedHeads[0] != "" {
		t.Fatal("publication list lost private receive evidence", listed)
	}
	copy := got.Clone()
	copy.ReceivedHeads[0] = "changed"
	if got.ReceivedHeads[0] != "" {
		t.Fatal("publication clone aliases receive evidence")
	}
	encoded, err := json.Marshal(got)
	requireOK(t, err)
	if strings.Contains(string(encoded), "received_heads") || strings.Contains(string(encoded), "ReceivedHeads") {
		t.Fatal("private receive evidence leaked into public JSON", string(encoded))
	}
}

func TestReceiveChainRebuildsBetweenAmends(t *testing.T) {
	s := testStore(t)
	_, input := accepted(t, s, "chain-template")
	r := input.Request.Clone()
	r.ReceiveID, r.PreviousSHA, r.ObjectSHA = "chain-first", strings.Repeat("b", 40), strings.Repeat("c", 40)
	r.CommitSHA = new(r.ObjectSHA)
	accept := func() {
		t.Helper()
		_, err := s.AcceptBatch(t.Context(), stateport.BatchAcceptance{ReceiveID: r.ReceiveID, Repository: r.Repo, Actor: r.Actor, Received: []model.CIRequest{r}})
		requireOK(t, err)
	}
	accept()
	path, options := s.path, s.options
	requireOK(t, s.Close())
	var err error
	s, err = Open(t.Context(), path, options)
	requireOK(t, err)
	defer func() { requireOK(t, s.Close()) }()
	r.ReceiveID, r.PreviousSHA, r.ObjectSHA = "chain-second", r.ObjectSHA, strings.Repeat("d", 40)
	r.CommitSHA = new(r.ObjectSHA)
	accept()
	// A replaced writer connection loses TEMP state without losing evidence.
	s.writer.SetMaxIdleConns(0)
	got, err := s.ReceivedHeads(t.Context(), stateport.ReceivedRef{Repo: r.Repo, Ref: r.Ref, ObjectSHA: r.ObjectSHA})
	requireOK(t, err)
	if !slices.Equal(got, []string{strings.Repeat("c", 40), strings.Repeat("b", 40)}) {
		t.Fatal("restart or connection replacement broke the chain", got)
	}
}

func TestReceiveChainSeesAnotherWriter(t *testing.T) {
	s := testStore(t)
	_, input := accepted(t, s, "writer-template")
	r := input.Request.Clone()
	r.ReceiveID, r.PreviousSHA, r.ObjectSHA = "writer-first", strings.Repeat("b", 40), strings.Repeat("c", 40)
	r.CommitSHA = new(r.ObjectSHA)
	accept := func(store *Store) {
		t.Helper()
		_, err := store.AcceptBatch(t.Context(), stateport.BatchAcceptance{ReceiveID: r.ReceiveID, Repository: r.Repo, Actor: r.Actor, Received: []model.CIRequest{r}})
		requireOK(t, err)
	}
	accept(s)
	other, err := Open(t.Context(), s.path, s.options)
	requireOK(t, err)
	defer func() { requireOK(t, other.Close()) }()
	r.ReceiveID, r.PreviousSHA, r.ObjectSHA = "writer-second", r.ObjectSHA, strings.Repeat("d", 40)
	r.CommitSHA = new(r.ObjectSHA)
	accept(other)
	got, err := s.ReceivedHeads(t.Context(), stateport.ReceivedRef{Repo: r.Repo, Ref: r.Ref, ObjectSHA: r.ObjectSHA})
	requireOK(t, err)
	if !slices.Equal(got, []string{strings.Repeat("c", 40), strings.Repeat("b", 40)}) {
		t.Fatal("sync ignored the other writer", got)
	}
	r.ReceiveID, r.PreviousSHA, r.ObjectSHA = "writer-third", r.ObjectSHA, strings.Repeat("e", 40)
	r.CommitSHA = new(r.ObjectSHA)
	accept(s)
	got, err = s.ReceivedHeads(t.Context(), stateport.ReceivedRef{Repo: r.Repo, Ref: r.Ref, ObjectSHA: r.ObjectSHA})
	requireOK(t, err)
	if !slices.Equal(got, []string{strings.Repeat("d", 40), strings.Repeat("c", 40), strings.Repeat("b", 40)}) {
		t.Fatal("receive ignored the other writer", got)
	}
}
