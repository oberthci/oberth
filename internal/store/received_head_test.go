package store

import (
	"encoding/json"
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
		got, err := s.ReceivedHead(t.Context(), stateport.ReceivedRef{Repo: q.Repo, Ref: q.Ref, ObjectSHA: q.ObjectSHA, RunID: tc.run})
		requireOK(t, err)
		if got == nil || *got != tc.want {
			t.Fatal("receive evidence changed", tc, got)
		}
	}
	missing, err := s.ReceivedHead(t.Context(), stateport.ReceivedRef{Repo: q.Repo, Ref: q.Ref, ObjectSHA: strings.Repeat("d", 40)})
	requireOK(t, err)
	if missing != nil {
		t.Fatal("another object inherited receive evidence", missing)
	}
}

func TestPublicationKeepsPrivateReceiveHeadThroughAttemptAndBind(t *testing.T) {
	s := testStore(t)
	r, _ := accepted(t, s, "receive-evidence")
	p := model.Publication{ID: "receive-evidence", Repo: r.Repo, Ref: r.Ref, Actor: r.Origin.Actor, ObjectSHA: r.ObjectSHA, CommitSHA: r.CommitSHA, ReceivedSHA: new("")}
	a := stateport.Action{Actor: p.Actor}
	requireOK(t, s.PreparePublication(t.Context(), a, stateport.PublicationIntent{Publication: p}))
	_, err := s.StartPublicationAttempt(t.Context(), a, stateport.PublicationAttempt{ID: p.ID})
	requireOK(t, err)
	got, err := s.BindPublication(t.Context(), a, p.ID, strings.Repeat("b", 40))
	requireOK(t, err)
	if got.ReceivedSHA == nil || *got.ReceivedSHA != "" {
		t.Fatal("known-absent receive head was lost", got)
	}
	listed, err := s.Publications(t.Context(), stateport.RecordFilter{Limit: 10})
	requireOK(t, err)
	if len(listed) != 1 || listed[0].ReceivedSHA == nil || *listed[0].ReceivedSHA != "" {
		t.Fatal("publication list lost private receive evidence", listed)
	}
	copy := got.Clone()
	*copy.ReceivedSHA = "changed"
	if *got.ReceivedSHA != "" {
		t.Fatal("publication clone aliases receive evidence")
	}
	encoded, err := json.Marshal(got)
	requireOK(t, err)
	if strings.Contains(string(encoded), "received_sha") {
		t.Fatal("private receive evidence leaked into public JSON", string(encoded))
	}
}
