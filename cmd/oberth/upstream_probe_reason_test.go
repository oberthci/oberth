package main

import (
	"errors"
	"testing"

	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/gitssh"
	"github.com/oberthci/oberth/internal/publicerr"
)

// upstream add and verify say what repo verify says: a first clone still
// running is reported as such, not as a failed check.
func TestUpstreamProbeNamesAnUnfinishedFirstClone(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{publicerr.New(publicerr.Unavailable, gitport.CloneUnfinishedReason, gitport.CloneUnfinishedRemedy, nil), gitport.CloneUnfinishedReason},
		{publicerr.New(publicerr.Unavailable, gitssh.HostKeyMismatchReason, "Check the host key.", nil), gitssh.HostKeyMismatchReason},
		{publicerr.New(publicerr.Forbidden, repositoryArchivedReason, "Unarchive the repository before probing it.", nil), repositoryArchivedReason},
		{&gitport.EmptyRepositoryError{}, gitport.EmptyForgeReason},
		{errors.New("forge refused"), repositoryProbeFailureReason},
	} {
		if got := upstreamProbeStatusReason(tc.err); got != tc.want {
			t.Fatal("upstream probe reason differs", got, tc.want)
		}
	}
}
