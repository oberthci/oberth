package contracttest

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
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
	for _, tc := range []struct{ run, object, want string }{{"received", r.ObjectSHA, r.PreviousSHA}, {"", later.ObjectSHA, later.PreviousSHA}} {
		expectReceivedHead(t, f, stateport.ReceivedRef{Repo: r.Repo, Ref: r.Ref, ObjectSHA: tc.object, RunID: tc.run}, new(tc.want))
	}
	q := stateport.ReceivedRef{Repo: r.Repo, Ref: r.Ref, ObjectSHA: r.ObjectSHA}
	expectReceivedHead(t, f, q, nil)
	later.ReceiveID, later.ObjectSHA, later.PreviousSHA = "created", strings.Repeat("d", 40), ""
	parked.ReceiveID, parked.Received = later.ReceiveID, []model.CIRequest{later}
	if _, err := f.Port.AcceptBatch(t.Context(), parked); err != nil {
		t.Fatal(err)
	}
	q.ObjectSHA = later.ObjectSHA
	expectReceivedHead(t, f, q, new(""))
}

func expectReceivedHead(t *testing.T, f RunFixture, q stateport.ReceivedRef, want *string) {
	t.Helper()
	got, err := f.Port.ReceivedHead(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	if want == nil {
		if got != nil {
			t.Fatal("another object inherited receive evidence", q, got)
		}
		return
	}
	if got == nil || *got != *want {
		t.Fatal("receive evidence was replaced or became unknown", q, got, *want)
	}
	*got = "caller edit"
}
