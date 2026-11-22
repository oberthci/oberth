package service_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
)

type trustCalls struct{ persisted, probed bool }

func confirmationFixture(t *testing.T) (*peopleFixture, *trustCalls) {
	t.Helper()
	p := people(t)
	p.f.c.People.Admin = p.admin
	calls := &trustCalls{}
	admin := p.f.c.People.Administration.(*testAdministration)
	admin.Prepare = func(context.Context, apicontract.UpstreamAddArgs) (service.PreparedUpstream, error) {
		return service.PreparedUpstream{Binding: stateport.Upstream{Name: "new", Org: "other", Kind: "ssh", BaseURL: "ssh://forge.oberth.ci", KeyName: "shared"}, Offered: []string{"new-pin"}, Replaced: []string{"old-pin"}, Persist: func(ctx context.Context) error {
			rows, err := p.state.AdministrativeAudit(ctx, nil, stateport.AdministrativeFilter{Action: "upstream.repin", Limit: 20})
			if err != nil || len(rows) != 1 {
				t.Fatal("pins preceded the audit", rows, err)
			}
			calls.persisted = true
			return nil
		}}, nil
	}
	admin.Probe = func(_ context.Context, name, repo string) (apicontract.Upstream, error) {
		if !calls.persisted || name != "new" || repo != "other/widget" {
			t.Fatal("explicit probe bypassed confirmation", name, repo)
		}
		calls.probed = true
		return apicontract.Upstream{Name: name}, nil
	}
	p.f.restart(t)
	return p, calls
}
func TestUpstreamConfirmationAuditsBeforePinsAndChecksExplicitRepository(t *testing.T) {
	t.Parallel()
	p, calls := confirmationFixture(t)
	q := apicontract.UpstreamAddArgs{Name: "new", BaseURL: "ssh://forge.oberth.ci", CheckRepo: "other/widget", NoWait: true, Repin: true}
	call, err := p.f.s.PrepareUpstreamAdd(t.Context(), adminCaller(), q)
	out, err := execute(t.Context(), call, err)
	if err != nil || !calls.persisted || !calls.probed || len(out.Replaced) != 1 || len(out.Offered) != 1 {
		t.Fatal("upstream wire missing", out, err)
	}
}
func TestUpstreamAuditRefusalDoesNotPersistPins(t *testing.T) {
	t.Parallel()
	p, calls := confirmationFixture(t)
	p.admin.Fail = errors.New("audit refused")
	q := apicontract.UpstreamAddArgs{Name: "new", BaseURL: "ssh://forge.oberth.ci", CheckRepo: "other/widget", NoWait: true, Repin: true}
	call, err := p.f.s.PrepareUpstreamAdd(t.Context(), adminCaller(), q)
	_, err = execute(t.Context(), call, err)
	if err == nil || calls.persisted || calls.probed {
		t.Fatal("audit refusal allowed a trust effect", err)
	}
}

func TestInvalidUpstreamAddressRefusesBeforeHandshake(t *testing.T) {
	t.Parallel()
	for _, address := range []string{"ssh://forge.oberth.ci?x=1", "ssh://forge.oberth.ci?", "ssh://forge.oberth.ci#x", "ssh://git:secret@forge.oberth.ci", "ssh://-oProxyCommand=x@forge.oberth.ci", "ssh://forge.oberth.ci/srv/../team", "ssh://forge.oberth.ci:99999", "ssh://forge.oberth.ci:0", "ssh://forge.oberth.ci:"} {
		t.Run(address, func(t *testing.T) {
			p, _ := confirmationFixture(t)
			p.f.c.People.Administration.(*testAdministration).Prepare = func(context.Context, apicontract.UpstreamAddArgs) (service.PreparedUpstream, error) {
				t.Fatal("invalid address reached handshake")
				return service.PreparedUpstream{}, nil
			}
			p.f.restart(t)
			call, err := p.f.s.PrepareUpstreamAdd(t.Context(), adminCaller(), apicontract.UpstreamAddArgs{Name: "new", BaseURL: address})
			_, err = execute(t.Context(), call, err)
			if err == nil || !strings.Contains(err.Error(), "base_url") {
				t.Fatal("bad address accepted or misdiagnosed", err)
			}
		})
	}
}

func TestFailedExplicitProbeLeavesCompletePendingRegistration(t *testing.T) {
	t.Parallel()
	p, calls := confirmationFixture(t)
	cause := "The forge repository has no commit."
	p.f.c.People.Administration.(*testAdministration).Probe = func(_ context.Context, _, repo string) (apicontract.Upstream, error) {
		if !calls.persisted {
			t.Fatal("probe preceded durable configuration")
		}
		out := apicontract.Upstream{Name: "new", Pending: true, DisplayReason: "Git SSH host key does not match the pin."}
		if repo != "" {
			return out, publicerr.New(publicerr.InvalidInput, cause, "Push the first commit with your own forge access, then retry.", nil)
		}
		return out, nil
	}
	p.f.restart(t)
	call, err := p.f.s.PrepareUpstreamAdd(t.Context(), adminCaller(), apicontract.UpstreamAddArgs{Name: "new", BaseURL: "ssh://forge.oberth.ci", CheckRepo: "other/widget", Repin: true})
	_, err = execute(t.Context(), call, err)
	problem := publicerr.Classify(err)
	if problem == nil || problem.Reason() != cause || !slices.Contains(problem.Context, "Upstream is registered: new") {
		t.Fatal("failed explicit check hid its cause or the registration", err)
	}
	checkPendingUpstreamList(t, p, false)
	checkPendingUpstreamList(t, p, true)
}

func checkPendingUpstreamList(t *testing.T, p *peopleFixture, checked bool) {
	t.Helper()
	list, err := p.f.s.PrepareUpstreamList(t.Context(), adminCaller(), apicontract.UpstreamListArgs{Check: checked})
	result, err := execute(t.Context(), list, err)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, upstream := range result.Upstreams {
		if upstream.Name != "new" {
			continue
		}
		found = true
		if !upstream.Pending || upstream.DisplayReason == "" {
			t.Fatal("unprobed list claimed verification", upstream)
		}
		if checked && upstream.DisplayReason != "Git SSH host key does not match the pin." {
			t.Fatal("pending reason was lost", upstream)
		}
	}
	if !found {
		t.Fatal("completed pending registration was lost", result)
	}
}
