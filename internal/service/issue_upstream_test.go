package service_test

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestFirstIssueUsesOnlyUpstreamInLiveScope(t *testing.T) {
	p := people(t)
	for _, scope := range [][]string{{"origin"}, nil} {
		a := caller()
		a.Scope = scope
		p.f.store.Actors[a.Fingerprint] = a
		call, err := p.f.s.PrepareIssueCreate(t.Context(), workerCaller(), apicontract.IssueCreateArgs{Title: "first task", Body: new("body")})
		result, err := execute(t.Context(), call, err)
		if err != nil || result.Issue == nil || result.Issue.Upstream != "origin" {
			t.Fatal("first issue did not select the visible upstream", result, err)
		}
	}
	call, err := p.f.s.PrepareIssueCreate(t.Context(), adminCaller(), apicontract.IssueCreateArgs{Title: "global task", Body: new("body")})
	result, err := execute(t.Context(), call, err)
	if err != nil || result.Issue.Upstream != "origin" {
		t.Fatal("administrator did not select the sole registered upstream", err)
	}
}

func TestIssueUpstreamRefusalNamesScopedChoices(t *testing.T) {
	p := people(t)
	if err := p.state.RegisterUpstream(t.Context(), stateport.Action{Actor: "owner"}, stateport.Upstream{Name: "second", Org: "other", Kind: "git", BaseURL: "https://second.oberth.ci", KeyName: "second"}); err != nil {
		t.Fatal(err)
	}
	a := caller()
	a.Scope = []string{"second", "origin"}
	p.f.store.Actors[a.Fingerprint] = a
	call, err := p.f.s.PrepareIssueCreate(t.Context(), workerCaller(), apicontract.IssueCreateArgs{Title: "task", Body: new("body")})
	_, err = execute(t.Context(), call, err)
	problem := publicerr.Classify(err)
	if err == nil || !strings.Contains(problem.Remedy(), "--upstream <name>") || strings.Join(problem.Context, "\n") != `Upstream choices: "origin", "second"` {
		t.Fatal("scope choice was not actionable", err)
	}
	if _, err = p.f.s.PrepareIssueCreate(t.Context(), workerCaller(), apicontract.IssueCreateArgs{Upstream: "global", Title: "task", Body: new("body")}); err == nil {
		t.Fatal("reserved global upstream was accepted")
	}
	call, err = p.f.s.PrepareIssueCreate(t.Context(), workerCaller(), apicontract.IssueCreateArgs{Upstream: "second", Title: "task", Body: new("body")})
	result, err := execute(t.Context(), call, err)
	if err != nil || result.Issue.Upstream != "second" {
		t.Fatal("printed remedy did not work", result, err)
	}
	a.Scope = []string{"unregistered"}
	p.f.store.Actors[a.Fingerprint] = a
	call, err = p.f.s.PrepareIssueCreate(t.Context(), workerCaller(), apicontract.IssueCreateArgs{Title: "task", Body: new("body")})
	_, err = execute(t.Context(), call, err)
	assertUnknownUpstreams(t, err, `Unknown upstreams: "unregistered"`)
}
