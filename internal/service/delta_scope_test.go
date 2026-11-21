package service_test

import (
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"strings"
	"testing"
)

func TestNamedRepositoryScopeHidesPushAndSelectors(t *testing.T) {
	t.Parallel()
	f := setup(t)
	hidden := "private/team/hidden"
	f.store.native.SeedRepository(stateport.Repository{Upstream: "private", Name: "hidden", Canonical: hidden})
	e := update()
	e.Repo = hidden
	_, err := f.s.Decide(t.Context(), e)
	requireCode(t, err, publicerr.NotFound)
	c := apicontract.Caller{Actor: caller()}
	_, err = f.s.Service.PrepareRunList(t.Context(), c, apicontract.RunListArgs{Repo: hidden})
	requireCode(t, err, publicerr.NotFound)
	_, err = f.s.PrepareRunCheck(t.Context(), c, apicontract.RunCheckArgs{Repo: hidden})
	requireCode(t, err, publicerr.NotFound)
	_, err = f.s.PrepareEventsWait(t.Context(), c, apicontract.EventsWaitArgs{Repos: []string{hidden}})
	requireCode(t, err, publicerr.NotFound)
	if f.store.Calls()["AcceptBatch"] != 0 {
		t.Fatal("hidden push reached acceptance")
	}
}
func TestCommitScopeHidesStatusWaitAndLogs(t *testing.T) {
	t.Parallel()
	f := setup(t)
	hidden := f.seed(stepmodel.RunPassed)
	hidden.ID = strings.Repeat("b", 32)
	hidden.Repo = "private/team/hidden"
	hidden.ObjectSHA = strings.Repeat("c", 40)
	hidden.CommitSHA = new(hidden.ObjectSHA)
	f.store.native.SeedRepository(stateport.Repository{Upstream: "private", Name: "hidden", Canonical: hidden.Repo})
	f.store.native.SeedRun(hidden)
	c := apicontract.Caller{Actor: caller()}
	prefix := hidden.ObjectSHA[:8]
	_, err := f.s.PrepareStatus(t.Context(), c, apicontract.StatusArgs{Repo: repository, Ref: prefix})
	requireCode(t, err, publicerr.NotFound)
	_, err = f.s.Service.PrepareWait(t.Context(), c, apicontract.WaitArgs{SHA: prefix})
	requireCode(t, err, publicerr.NotFound)
	_, err = f.s.Service.PrepareLogs(t.Context(), c, apicontract.LogsArgs{SHA: prefix})
	requireCode(t, err, publicerr.NotFound)
}
func TestDecideRefusesEmptyReceiveIdentity(t *testing.T) {
	t.Parallel()
	f := setup(t)
	e := update()
	e.ID = ""
	_, err := f.s.Decide(t.Context(), e)
	requireField(t, err, "receive_id")
}
func TestDecideRefusesInvalidQuarantineIdentities(t *testing.T) {
	t.Parallel()
	for _, which := range []string{"ref", "object", "commit"} {
		t.Run(which, func(t *testing.T) {
			f := setup(t)
			e := update()
			switch which {
			case "ref":
				e.Ref = "invalid"
			case "object":
				e.ObjectSHA = "short"
			case "commit":
				e.CommitSHA = ""
			}
			_, err := f.s.Decide(t.Context(), e)
			requireField(t, err, "ref/object_sha/commit_sha")
		})
	}
}
