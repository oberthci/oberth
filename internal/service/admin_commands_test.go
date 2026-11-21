package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stepmodel"
)

// requireCommand checks that a refusal names exactly this administrator
// command and that every transport's JSON carries it as a context line.
func requireCommand(t *testing.T, err error, want string) {
	t.Helper()
	var p *publicerr.Error
	if !errors.As(err, &p) || p.Command() != want {
		t.Fatalf("want command %q, got %v", want, err)
	}
	raw, _ := json.Marshal(publicerr.Classify(err))
	var public struct{ Context []string }
	if json.Unmarshal(raw, &public) != nil || !slices.Contains(public.Context, publicerr.AdministratorLine(want)) {
		t.Fatalf("command missing from the public refusal: %s", raw)
	}
}

func TestArchivedRefusalsCarryTheUnarchiveCommand(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.seed(stepmodel.RunPassed)
	f.archiveRepository(t, repository)
	want := "oberth repo unarchive 'origin/team/widget'"
	_, err := f.s.Decide(t.Context(), update())
	requireCommand(t, err, want)
	_, err = f.s.PreparePromote(t.Context(), workerCaller(), apicontract.PromoteArgs{Repo: repository, SHA: head})
	requireCommand(t, err, want)
	_, err = f.s.PrepareSync(t.Context(), workerCaller(), apicontract.SyncArgs{Repo: repository, Branch: "topic", SHA: head})
	requireCommand(t, err, want)
}

func TestUplinkPromotionRefusalCarriesTheAdministratorPromotion(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.seed(stepmodel.RunPassed)
	base := strings.Repeat("c", 40)
	f.source.File = func(context.Context, string, string) (gitport.Ref, gitport.File, error) {
		return gitport.Ref{Name: "refs/heads/main", ObjectSHA: base, CommitSHA: base}, gitport.File{Bytes: []byte("uplinks_may_promote: false")}, nil
	}
	_, err := f.s.PreparePromote(t.Context(), apicontract.Caller{Actor: caller()}, apicontract.PromoteArgs{Repo: repository, SHA: head})
	requireCode(t, err, publicerr.Forbidden)
	requireCommand(t, err, "oberth promote '"+head+"' --repo 'origin/team/widget'")
	_, err = f.s.PreparePromote(t.Context(), apicontract.Caller{Actor: caller()}, apicontract.PromoteArgs{Repo: repository, SHA: "not-a-commit"})
	var p *publicerr.Error
	if !errors.As(err, &p) || p.Code() != publicerr.Forbidden || p.Command() != "" {
		t.Fatal("an invalid commit was copied into a command", err)
	}
}

func TestUngrantedSecretPutCarriesTheExactGrant(t *testing.T) {
	t.Parallel()
	p := people(t)
	p.f.c.People.SecretWriter = secretWriterFunc(func(context.Context, string, string, []byte) error {
		t.Error("ungranted secret written")
		return nil
	})
	p.f.restart(t)
	_, err := putSecret(t, p, secretTarget(), []byte("private-body-marker"))
	requireCode(t, err, publicerr.Forbidden)
	requireCommand(t, err, "oberth access allow 'origin/team/widget' '.oberth/build.yaml' 'build' 'registry/key'")
	if strings.Contains(err.Error(), "private-body-marker") {
		t.Fatal("secret value reached the refusal")
	}
}
