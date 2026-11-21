package service_test

import (
	"slices"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

const secondRepository = "second/other/widget"

// upstreamRuns has one run under origin (the newest) and one under second.
func upstreamRuns(t *testing.T) *peopleFixture {
	t.Helper()
	p := people(t)
	if err := p.state.RegisterUpstream(t.Context(), stateport.Action{Actor: "owner"}, stateport.Upstream{Name: "second", Org: "other", Kind: "git", BaseURL: "https://second.oberth.ci", KeyName: "second"}); err != nil {
		t.Fatal(err)
	}
	p.f.store.native.SeedRepository(stateport.Repository{ID: 2, Upstream: "second", Name: "widget", Canonical: secondRepository})
	older := p.f.seed(stepmodel.RunPassed)
	older.ID, older.Repo, older.Sequence = "abcdef01000000000000000000000002", secondRepository, 0
	p.f.store.native.SeedRun(older)
	p.f.seed(stepmodel.RunRunning)
	return p
}

func TestRunListNarrowsToUpstreamsBeforePagination(t *testing.T) {
	t.Parallel()
	p := upstreamRuns(t)
	for _, tc := range []struct {
		name string
		q    viewQuery
		want []string
	}{
		{"absent means every upstream", viewQuery{}, []string{repository, secondRepository}},
		{"one upstream", viewQuery{Upstreams: []string{"origin"}}, []string{repository}},
		{"older run on the first page", viewQuery{Upstreams: []string{"second"}, Limit: 1}, []string{secondRepository}},
		{"both upstreams", viewQuery{Upstreams: []string{"second", "origin"}}, []string{repository, secondRepository}},
		{"repository inside the upstreams", viewQuery{Repo: repository, Upstreams: []string{"origin"}}, []string{repository}},
		{"repository outside the upstreams", viewQuery{Repo: repository, Upstreams: []string{"second"}}, []string{}},
	} {
		page, err := p.f.s.RunList(t.Context(), admin(), tc.q)
		if err != nil {
			t.Fatal(tc.name, err)
		}
		got := []string{}
		for _, run := range page.Runs {
			got = append(got, run.Repo)
		}
		slices.Sort(got)
		if !slices.Equal(got, tc.want) {
			t.Fatal(tc.name, got)
		}
	}
}

func TestRunListRefusesUnknownUpstreams(t *testing.T) {
	t.Parallel()
	p := upstreamRuns(t)
	_, err := p.f.s.RunList(t.Context(), admin(), viewQuery{Upstreams: []string{"origin", "missing"}})
	assertUnknownUpstreams(t, err, `Unknown upstreams: "missing"`)
	problem := publicerr.Classify(err)
	if len(problem.Reason()) > 80 || len(problem.Remedy()) > 120 {
		t.Fatal("refusal is too long to read whole", problem.Reason(), problem.Remedy())
	}
	// A registered upstream outside the caller's scope is refused like an unknown
	// one, so a scoped caller cannot tell which other upstreams exist.
	_, err = p.f.s.RunList(t.Context(), caller(), viewQuery{Upstreams: []string{"second"}})
	assertUnknownUpstreams(t, err, `Unknown upstreams: "second"`)
	page, err := p.f.s.RunList(t.Context(), caller(), viewQuery{Upstreams: []string{"origin"}})
	if err != nil || len(page.Runs) != 1 || page.Runs[0].Repo != repository {
		t.Fatal("scoped caller lost its own upstream", page, err)
	}
}
