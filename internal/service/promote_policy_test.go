package service_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stepmodel"
)

// legacyPolicy carries fields this version does not know beside one that
// would widen promotion if it were read.
const legacyPolicy = "allow_without_branch_ci: true\nuplinks_may_promote: true\npark_ref_prefix: refs/heads/park/\nrequire_branch_ci: false\n"

func legacyPolicyFixture(t *testing.T, body string) *fixture {
	t.Helper()
	f := setup(t)
	f.seed(stepmodel.RunPassed)
	base := strings.Repeat("b", 40)
	f.source.File = func(context.Context, string, string) (gitport.Ref, gitport.File, error) {
		return gitport.Ref{Name: "refs/heads/main", ObjectSHA: base, CommitSHA: base}, gitport.File{Bytes: []byte(body)}, nil
	}
	return f
}

func preparePromotion(t *testing.T, f *fixture, a actor.Actor, sha string) (apicontract.Call[apicontract.PromoteResult], error) {
	t.Helper()
	return f.s.PreparePromote(t.Context(), apicontract.Caller{Actor: a}, apicontract.PromoteArgs{Repo: repository, SHA: sha, Reason: "correct the policy"})
}

func TestInvalidPolicyLetsAdministratorPromoteTestedFix(t *testing.T) {
	t.Parallel()
	for _, body := range []string{legacyPolicy, "uplinks_may_promote: true\n---\nallow_without_branch_ci: true\n"} {
		f := legacyPolicyFixture(t, body)
		call, err := preparePromotion(t, f, admin(), head)
		if err != nil {
			t.Fatal("administrator could not promote the tested fix", err)
		}
		out, err := call(t.Context())
		if err != nil || out.TestedRunID != runID || out.CommitSHA != head {
			t.Fatal("tested fix did not promote on its branch evidence", out, err)
		}
	}
}

func TestInvalidPolicyNeverWidensPromotion(t *testing.T) {
	t.Parallel()
	f := legacyPolicyFixture(t, legacyPolicy)
	untested := strings.Repeat("f", 40)
	if call, err := preparePromotion(t, f, admin(), untested); err == nil || call != nil {
		t.Fatal("invalid policy allowed an untested promotion", err)
	}
	r := f.seed(stepmodel.RunFailed)
	if call, err := preparePromotion(t, f, admin(), *r.CommitSHA); err == nil || call != nil {
		t.Fatal("invalid policy allowed a red promotion", err)
	}
	if len(f.store.native.PublicationValues()) != 0 {
		t.Fatal("refused promotion wrote an intent")
	}
}

func TestInvalidPolicyRefusesUplinkWithAdministratorCommand(t *testing.T) {
	t.Parallel()
	f := legacyPolicyFixture(t, legacyPolicy)
	call, err := preparePromotion(t, f, caller(), head)
	if call != nil {
		t.Fatal("uplink promoted under an invalid policy")
	}
	requireCode(t, err, publicerr.InvalidInput)
	var p *publicerr.Error
	errors.As(err, &p)
	want := "oberth promote '" + head + "' --repo '" + repository + "'"
	if p.Command() != want || !strings.Contains(p.Remedy(), "administrator") {
		t.Fatalf("remedy %q command %q", p.Remedy(), p.Command())
	}
	_, err = preparePromotion(t, f, caller(), "")
	if !errors.As(err, &p) || p.Command() != "" {
		t.Fatal("a command was printed without an exact commit", err)
	}
}
