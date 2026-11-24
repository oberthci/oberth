package contracttest

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func checkReceivedHeads(t *testing.T, f RunFixture) {
	b := batchFor(t, f, RunCase{ID: "received", Ref: "refs/heads/topic"})
	r := b.Runs[0].Request.Clone()
	r.PreviousSHA = strings.Repeat("b", 40)
	b.Runs[0].Request = r.Clone()
	b.Received = []model.CIRequest{r}
	if _, err := f.Port.AcceptBatch(t.Context(), b); err != nil {
		t.Fatal(err)
	}
	later := r.Clone()
	later.ReceiveID, later.ObjectSHA, later.PreviousSHA = "parked", strings.Repeat("c", 40), r.ObjectSHA
	parked := stateport.BatchAcceptance{ReceiveID: later.ReceiveID, Repository: later.Repo, Actor: later.Actor, Received: []model.CIRequest{later}}
	if _, err := f.Port.AcceptBatch(t.Context(), parked); err != nil {
		t.Fatal(err)
	}
	if result, err := f.Port.AcceptBatch(t.Context(), b); err != nil || result.Fresh {
		t.Fatal("replay changed receive evidence", result, err)
	}
	for _, tc := range []struct {
		run, object string
		want        []string
	}{{"received", r.ObjectSHA, []string{r.PreviousSHA}}, {"", later.ObjectSHA, []string{later.PreviousSHA, r.PreviousSHA}}} {
		expectReceivedHeads(t, f, stateport.ReceivedRef{Repo: r.Repo, Ref: r.Ref, ObjectSHA: tc.object, RunID: tc.run}, tc.want)
	}
	q := stateport.ReceivedRef{Repo: r.Repo, Ref: r.Ref, ObjectSHA: r.ObjectSHA}
	expectReceivedHeads(t, f, q, nil)
	later.ReceiveID, later.ObjectSHA, later.PreviousSHA = "created", strings.Repeat("d", 40), ""
	parked.ReceiveID, parked.Received = later.ReceiveID, []model.CIRequest{later}
	if _, err := f.Port.AcceptBatch(t.Context(), parked); err != nil {
		t.Fatal(err)
	}
	q.ObjectSHA = later.ObjectSHA
	expectReceivedHeads(t, f, q, []string{""})
}

func expectReceivedHeads(t *testing.T, f RunFixture, q stateport.ReceivedRef, want []string) {
	t.Helper()
	got, err := f.Port.ReceivedHeads(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	if want == nil {
		if got != nil {
			t.Fatal("another object inherited receive evidence", q, got)
		}
		return
	}
	if !slices.Equal(got, want) {
		t.Fatal("receive evidence was replaced or became unknown", q, got, want)
	}
	got[0] = "caller edit"
}

func checkReceivedChain(t *testing.T, f RunFixture) {
	b := batchFor(t, f, RunCase{ID: "chain", Ref: "refs/heads/topic"})
	r := b.Runs[0].Request.Clone()
	b.Runs = nil
	heads := []string{strings.Repeat("b", 40)}
	for i := 0; i < stateport.MaxReceivedHeads+2; i++ {
		r.ReceiveID = fmt.Sprintf("chain-%d", i)
		r.PreviousSHA = heads[0]
		r.ObjectSHA = fmt.Sprintf("%040x", i+1)
		r.CommitSHA = new(r.ObjectSHA)
		b.ReceiveID, b.Received = r.ReceiveID, []model.CIRequest{r}
		_, err := f.Port.AcceptBatch(t.Context(), b)
		mustRun(t, err)
		expectReceivedHeads(t, f, stateport.ReceivedRef{Repo: r.Repo, Ref: r.Ref, ObjectSHA: r.ObjectSHA}, heads)
		heads = append([]string{r.ObjectSHA}, heads...)
		if len(heads) > stateport.MaxReceivedHeads {
			heads = heads[:stateport.MaxReceivedHeads]
		}
	}
	// A mismatch starts a fresh set; it cannot borrow a previous receive's root.
	r.ReceiveID, r.PreviousSHA, r.ObjectSHA = "broken-chain", strings.Repeat("e", 40), strings.Repeat("f", 40)
	r.CommitSHA = new(r.ObjectSHA)
	b.ReceiveID, b.Received = r.ReceiveID, []model.CIRequest{r}
	_, err := f.Port.AcceptBatch(t.Context(), b)
	mustRun(t, err)
	q := stateport.ReceivedRef{Repo: r.Repo, Ref: r.Ref, ObjectSHA: r.ObjectSHA}
	expectReceivedHeads(t, f, q, []string{r.PreviousSHA})
	// A late event failure must roll back both the receipt and its derived index.
	event := runEvent(stateport.Run{RunSummary: stepmodel.RunSummary{Repo: r.Repo}}, "chain-collision")
	mustRun(t, f.Emit(t.Context(), []stateport.EventWrite{event}))
	event.Event.Summary = "changed"
	b.Events = []stateport.EventWrite{event}
	r.ReceiveID, r.PreviousSHA, r.ObjectSHA = "failed-chain", r.ObjectSHA, strings.Repeat("d", 40)
	r.CommitSHA = new(r.ObjectSHA)
	b.ReceiveID, b.Received = r.ReceiveID, []model.CIRequest{r}
	_, err = f.Port.AcceptBatch(t.Context(), b)
	runCode(t, err, publicerr.Conflict)
	expectReceivedHeads(t, f, q, []string{strings.Repeat("e", 40)})
	q.ObjectSHA = r.ObjectSHA
	expectReceivedHeads(t, f, q, nil)
	b.Events = nil
	_, err = f.Port.AcceptBatch(t.Context(), b)
	mustRun(t, err)
	expectReceivedHeads(t, f, q, []string{r.PreviousSHA, strings.Repeat("e", 40)})
}
