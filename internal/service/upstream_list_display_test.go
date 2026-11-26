package service_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
)

func TestUpstreamListsOmitUncheckedPendingAndKeepProbeResults(t *testing.T) {
	p := people(t)
	p.f.c.People.Administration.(*testAdministration).Probe = func(_ context.Context, name, _ string) (apicontract.Upstream, error) {
		return apicontract.Upstream{Name: name, Pending: new(false), DisplayReason: "Repository is readable."}, nil
	}
	p.f.restart(t)
	list, err := p.f.s.PrepareUpstreamList(t.Context(), adminCaller(), apicontract.UpstreamListArgs{})
	unchecked, err := execute(t.Context(), list, err)
	if err != nil || len(unchecked.Upstreams) == 0 {
		t.Fatal("unchecked list", unchecked, err)
	}
	repos, err := p.f.s.PrepareRepoList(t.Context(), adminCaller(), apicontract.RepoListArgs{})
	page, err := execute(t.Context(), repos, err)
	if err != nil || len(page.Upstreams) != len(unchecked.Upstreams) {
		t.Fatal("repository upstreams", page.Upstreams, err)
	}
	requireUncheckedUpstreams(t, unchecked.Upstreams)
	requireUncheckedUpstreams(t, page.Upstreams)
	list, err = p.f.s.PrepareUpstreamList(t.Context(), adminCaller(), apicontract.UpstreamListArgs{Check: true})
	checked, err := execute(t.Context(), list, err)
	if err != nil || len(checked.Upstreams) != len(unchecked.Upstreams) {
		t.Fatal("checked list", checked, err)
	}
	for _, upstream := range checked.Upstreams {
		if upstream.Pending == nil || *upstream.Pending || upstream.DisplayReason != "Repository is readable." {
			t.Fatal("successful check lost its result", upstream)
		}
	}
}

func requireUncheckedUpstreams(t *testing.T, rows []apicontract.Upstream) {
	t.Helper()
	for _, upstream := range rows {
		raw, _ := json.Marshal(upstream)
		var fields map[string]any
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		if _, present := fields["pending"]; present || fields["display_reason"] != nil {
			t.Fatal("unchecked upstream claimed a check result", string(raw))
		}
	}
}
