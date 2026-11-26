package main

import (
	"errors"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/gitssh"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
)

// upstream list --check reads pending: a failure after a readable repository
// still leaves the upstream pending with the failure's reason.
func TestUpstreamCheckStaysPendingAfterALaterFailure(t *testing.T) {
	repos := []stateport.Repository{{Canonical: "origin/team/ok"}, {Canonical: "origin/team/empty"}}
	check := func(r stateport.Repository) error {
		if r.Canonical == "origin/team/empty" {
			return &gitport.EmptyRepositoryError{}
		}
		return nil
	}
	out, err := checkEach(apicontract.Upstream{Pending: new(true)}, repos, check, false)
	if err != nil || out.Pending == nil || !*out.Pending || out.DisplayReason != gitport.EmptyForgeReason {
		t.Fatal("a failed repository left the upstream verified", out, err)
	}
	if out, err = checkEach(apicontract.Upstream{Pending: new(true)}, repos[:1], check, false); err != nil || out.Pending == nil || *out.Pending {
		t.Fatal("a readable upstream stayed pending", out, err)
	}
}

// upstream add and verify say what repo verify says: a first clone still
// running is reported as such, not as a failed check.
func TestUpstreamProbeNamesAnUnfinishedFirstClone(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{publicerr.New(publicerr.Unavailable, gitport.CloneUnfinishedReason, gitport.CloneUnfinishedRemedy, nil), gitport.CloneUnfinishedReason},
		{&gitport.FetchUnfinishedError{}, gitport.FetchUnfinishedReason},
		{publicerr.New(publicerr.Unavailable, gitssh.HostKeyMismatchReason, "Check the host key.", nil), gitssh.HostKeyMismatchReason},
		{publicerr.New(publicerr.Forbidden, repositoryArchivedReason, "Unarchive the repository before probing it.", nil), repositoryArchivedReason},
		{&gitport.EmptyRepositoryError{}, gitport.EmptyForgeReason},
		{&gitport.ForgeHistoryLostError{}, gitport.ForgeHistoryLostReason},
		{errors.New("forge refused"), repositoryProbeFailureReason},
	} {
		if got := upstreamProbeStatusReason(tc.err); got != tc.want {
			t.Fatal("upstream probe reason differs", got, tc.want)
		}
	}
}
