package app_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/sourcecontract"
)

func pinFixture(t *testing.T, policy, producer string) *fixture {
	t.Helper()
	f := newFixture(t)
	f.d.SourcePolicy = []byte(policy)
	w := workflow()
	addSource(w, sourcecontract.Descriptor{Version: 1, Kind: sourcecontract.Pinned, Repository: producer, Commit: strings.Repeat("e", 40)})
	f.setDocument(w)
	f.source.Reachable = func(context.Context, sourcecontract.Revision) (int64, bool, error) { return 2, true, nil }
	return f
}

// The push line and the public refusal that check --local prints both carry
// the administrator command that allows exactly the refused pair.
func TestPinRefusalCarriesTheAdministratorCommand(t *testing.T) {
	t.Parallel()
	command := "oberth upgrade --version 'v0.30.3' --source-pin 'origin/team/widget=origin/team/source'"
	f := pinFixture(t, "{}", "origin/team/source")
	f.d.Version = "v0.30.3"
	s := f.new(t)
	_, err := s.Admit(context.Background(), request(), f.source)
	expectRefusal(t, err, "pin")
	if !strings.Contains(err.Error(), "source producer is not allowed.\n  fix: Administrator: "+command) {
		t.Fatalf("push line lacks the command:\n%v", err)
	}
	_, err = s.Check(context.Background(), request())
	var public *publicerr.Error
	if !errors.As(err, &public) || public.Command() != command || public.Reason() != "source producer is not allowed" {
		t.Fatalf("check refusal lacks the command: %v", err)
	}
}

// A repository pinning an older commit of itself is a pair like any other:
// refused, with its own command, until the policy lists it.
func TestSelfPinNeedsItsOwnPolicyPair(t *testing.T) {
	t.Parallel()
	self := request().Repository
	f := pinFixture(t, `{"origin/team/widget":["origin/team/source"]}`, self)
	_, err := f.new(t).Admit(context.Background(), request(), f.source)
	expectRefusal(t, err, "pin")
	if !strings.Contains(err.Error(), "Administrator: oberth upgrade --source-pin 'origin/team/widget=origin/team/widget'") {
		t.Fatalf("self-pin refusal lacks its command:\n%v", err)
	}
	f = pinFixture(t, `{"origin/team/widget":["origin/team/widget"]}`, self)
	p, err := f.new(t).Admit(context.Background(), request(), f.source)
	if err != nil {
		t.Fatal("listed self-pin refused", err)
	}
	if b := p.View().SourceBindings[0]; b.Revision.Repository != self || b.Revision.Commit != strings.Repeat("e", 40) {
		t.Fatal("self-pin bound another revision", b)
	}
}
