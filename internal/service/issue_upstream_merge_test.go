package service_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
)

type countedIssueUpstreams struct {
	service.AdminStore
	reads int
}

func (s *countedIssueUpstreams) UpstreamBindings(ctx context.Context) ([]stateport.Upstream, error) {
	s.reads++
	return s.AdminStore.UpstreamBindings(ctx)
}

func TestIssueCreateMergedUpstreamPrecedence(t *testing.T) {
	for _, real := range []bool{false, true} {
		for _, mode := range []string{"explicit", "repository", "uplink", "choices", "unknown", "removed-scope"} {
			t.Run(fmt.Sprint(real)+"/"+mode, func(t *testing.T) { checkMergedIssueUpstream(t, real, mode) })
		}
	}
}

func checkMergedIssueUpstream(t *testing.T, real bool, mode string) {
	t.Helper()
	p := issueUpstreamPeople(t, real)
	c, q, want := mergedIssueCreateArgs(t, p, mode)
	reads := &countedIssueUpstreams{AdminStore: p.f.c.People.Admin}
	p.f.c.People.Admin = reads
	p.f.restart(t)
	call, err := p.f.s.PrepareIssueCreate(t.Context(), c, q)
	result, err := execute(t.Context(), call, err)
	if reads.reads != 1 {
		t.Fatal("creation must read upstreams once", reads.reads)
	}
	switch mode {
	case "choices":
		if err == nil || !strings.Contains(strings.Join(publicerr.Classify(err).Context, " "), `Upstream choices: "origin", "private"`) {
			t.Fatal("ambiguous repository upstreams were not explained", err)
		}
	case "unknown", "removed-scope":
		assertUnknownUpstreams(t, err, `Unknown upstreams: "missing"`)
	default:
		if err != nil || result.Issue == nil || result.Issue.Upstream != want {
			t.Fatal("upstream precedence", result, err)
		}
	}
}

func mergedIssueCreateArgs(t *testing.T, p *peopleFixture, mode string) (apicontract.Caller, apicontract.IssueCreateArgs, string) {
	t.Helper()
	c := adminCaller()
	q := apicontract.IssueCreateArgs{Title: "task", Body: new("body")}
	want := "origin"
	if mode != "repository" {
		registerIssueUpstream(t, p, "private")
	}
	switch mode {
	case "explicit":
		q.Upstream, want = "private", "private"
	case "uplink":
		c = workerCaller()
		c.Actor.Scope = []string{"private"}
		p.f.store.Actors[c.Actor.Fingerprint] = c.Actor
		want = "private"
	case "unknown":
		q.Upstream = "missing"
	case "removed-scope":
		c = workerCaller()
		c.Actor.Scope = []string{"missing"}
		p.f.store.Actors[c.Actor.Fingerprint] = c.Actor
	}
	return c, q, want
}
