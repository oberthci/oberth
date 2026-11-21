package service_test

import (
	"context"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stepmodel"
)

// Promoting the commit the default branch already is says exactly that,
// before any evidence check could send the person in a circle.
func TestPromotingTheCurrentDefaultCommitSaysSo(t *testing.T) {
	t.Parallel()
	f := setup(t)
	r := f.seed(stepmodel.RunFailed)
	f.store.native.SeedRun(r)
	f.source.File = func(context.Context, string, string) (gitport.Ref, gitport.File, error) {
		return gitport.Ref{Name: "refs/heads/main", ObjectSHA: head, CommitSHA: head}, gitport.File{Bytes: []byte("uplinks_may_promote: true\n")}, nil
	}
	_, err := f.s.PreparePromote(t.Context(), apicontract.Caller{Actor: caller()}, apicontract.PromoteArgs{Repo: repository, SHA: head})
	requireCode(t, err, publicerr.InvalidTransition)
	if p := publicerr.Classify(err); p.Reason() != "The default branch is already at this commit." {
		t.Fatal("wrong refusal for a promotion of the current default commit", p.Reason(), p.Remedy())
	}
	if len(f.store.native.PublicationValues()) != 0 {
		t.Fatal("refused promotion wrote an intent")
	}
}
