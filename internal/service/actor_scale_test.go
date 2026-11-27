package service_test

import (
	"fmt"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestActorScaleUpstreamAuthority(t *testing.T) {
	f := setup(t)
	base := f.seed(stepmodel.RunPassed)
	for i, repo := range []string{"origin/team/other", "foreign/team/hidden"} {
		r := base
		r.ID, r.Repo = fmt.Sprintf("%032x", i+200), repo
		upstream := "origin"
		if i == 1 {
			upstream = "foreign"
		}
		f.store.native.SeedRepository(stateport.Repository{ID: int64(i + 2), Canonical: repo, Upstream: upstream, Name: fmt.Sprint(i)})
		f.store.native.SeedRun(r)
		f.store.native.SeedEvent(model.AgentEvent{ID: int64(i + 1), Kind: "run.finished", Repo: repo, RunID: r.ID})
		f.store.native.SeedEvent(model.AgentEvent{Kind: "issue.updated", Upstream: upstream, IssueID: int64(i + 1)})
	}
	for i := range 100 {
		checkScaleActorAuthority(t, f, i)
	}
}

// Upstream scope intentionally includes both origin repositories; own is only a filter.
func checkScaleActorAuthority(t *testing.T, f *fixture, i int) {
	t.Helper()
	a := caller()
	a.Identity, a.Fingerprint = fmt.Sprintf("actor-%d", i), fmt.Sprintf("credential-%d", i)
	f.store.Actors[a.Fingerprint] = a
	c := apicontract.Caller{Actor: a, CredentialID: a.Fingerprint}
	list, err := f.s.Service.PrepareRunList(t.Context(), c, apicontract.RunListArgs{})
	if err != nil {
		t.Fatal(err)
	}
	page, err := list(t.Context())
	if err != nil || len(page.Runs) != 2 {
		t.Fatal("upstream should expose both repositories", page, err)
	}
	_, err = f.s.PrepareRunGet(t.Context(), c, apicontract.RunGetArgs{ID: fmt.Sprintf("%032x", 201)})
	requireCode(t, err, publicerr.NotFound)
	wait, err := f.s.PrepareEventsWait(t.Context(), c, apicontract.EventsWaitArgs{AfterID: new(int64(0)), Snapshot: true})
	if err != nil {
		t.Fatal(err)
	}
	events, err := wait(t.Context())
	if err != nil || len(events.Events) != 2 {
		t.Fatal("event scope crossed upstream", events, err)
	}
	for _, event := range events.Events {
		if event.Repo != "origin/team/other" && event.Upstream != "origin" {
			t.Fatal("foreign run/issue result", event)
		}
	}
}
