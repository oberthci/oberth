package service_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/publicerr"
)

func TestIssueDeletionRequiresCreatorOrAdministrator(t *testing.T) {
	for _, real := range []bool{false, true} {
		t.Run(fmt.Sprint(real), func(t *testing.T) {
			checkIssueOwnership(t, real)
		})
	}
}

func checkIssueOwnership(t *testing.T, real bool) {
	p := people(t)
	if !real {
		p.f.c.People.Issues = p.f.store.native
		p.f.restart(t)
	}
	foreign := createTask(t, p, "origin", "retained")
	del, err := p.f.s.PrepareIssueDelete(t.Context(), workerCaller(), apicontract.IssueDeleteArgs{ID: foreign.ID})
	_, err = execute(t.Context(), del, err)
	requireCode(t, err, publicerr.Forbidden)
	if !strings.Contains(publicerr.Classify(err).Remedy(), "Close the issue instead") {
		t.Fatal(err)
	}
	read, err := p.f.s.PrepareIssueGet(t.Context(), workerCaller(), apicontract.IssueGetArgs{ID: foreign.ID})
	kept, err := execute(t.Context(), read, err)
	if err != nil || kept.Body != "retained" {
		t.Fatal(kept, err)
	}
	create, err := p.f.s.PrepareIssueCreate(t.Context(), workerCaller(), apicontract.IssueCreateArgs{Upstream: "origin", Title: "own", Body: new("body")})
	own, err := execute(t.Context(), create, err)
	if err != nil || own.Issue.Author != workerCaller().Actor.Identity {
		t.Fatal(own, err)
	}
	for _, tc := range []struct {
		caller apicontract.Caller
		id     int64
	}{{workerCaller(), own.ID}, {adminCaller(), foreign.ID}} {
		del, err = p.f.s.PrepareIssueDelete(t.Context(), tc.caller, apicontract.IssueDeleteArgs{ID: tc.id})
		result, e := execute(t.Context(), del, err)
		if e != nil || !result.Deleted {
			t.Fatal(result, e)
		}
	}
	c := workerCaller()
	c.Actor.Scope = nil
	p.f.store.Actors[c.Actor.Fingerprint] = c.Actor
	create, err = p.f.s.PrepareIssueCreate(t.Context(), c, apicontract.IssueCreateArgs{Upstream: "unregistered", Title: "refused", Body: new("")})
	_, err = execute(t.Context(), create, err)
	assertUnknownUpstreams(t, err, `Unknown upstreams: "unregistered"`)
}
