package service_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestReceivePolicyInGate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		a           actor.Actor
		run         bool
		ref, action string
		release     bool
	}{{"agent parks", caller(), false, "refs/heads/topic", "parked", false}, {"agent runs", caller(), true, "refs/heads/topic", "run", false}, {"operator runs", admin(), false, "refs/heads/topic", "run", false}, {"ordinary tag", caller(), false, "refs/tags/check", "run", false}, {"version tag", caller(), false, "refs/tags/v1.2.3", "run", true}, {"not a version", caller(), false, "refs/tags/v1.2.3junk", "run", false}, {"prerelease", caller(), false, "refs/tags/v1.2.3-rc.1", "run", false}, {"extra component", caller(), false, "refs/tags/v1.2.3.4", "run", false}, {"deletion", caller(), false, "refs/heads/topic", "deleted", false}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t)
			e := update()
			e.Actor = tc.a
			e.Run = tc.run
			e.Ref = tc.ref
			if tc.name == "deletion" {
				e.ObjectSHA = ""
			}
			d, err := f.s.Decide(t.Context(), e)
			if err != nil || d.Action != tc.action || d.Release != tc.release {
				t.Fatalf("decision %+v %v", d, err)
			}
			if len(f.store.native.Audits()) != 0 || f.store.Calls()["AcceptBatch"] != 0 {
				t.Fatal("gate decision persisted or submitted work")
			}
		})
	}
}
func TestDefaultBranchAgentRefusedWithPromoteRemedy(t *testing.T) {
	t.Parallel()
	f := setup(t)
	e := update()
	e.Ref = "refs/heads/main"
	_, err := f.s.Decide(t.Context(), e)
	requireCode(t, err, publicerr.Forbidden)
	if !strings.Contains(err.Error(), "promote") {
		t.Fatal(err)
	}
}
func TestVersionTagNeedsFreshAncestry(t *testing.T) {
	t.Parallel()
	f := setup(t)
	refused := errors.New("ancestry unreadable")
	f.git.Release = func(context.Context, stateport.Repository, gitport.Ref) error { return refused }
	e := update()
	e.Ref = "refs/tags/v1.2.3"
	_, err := f.s.Decide(t.Context(), e)
	if !errors.Is(err, refused) {
		t.Fatal(err)
	}
}
func TestReceivePolicyReadErrorRefuses(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.store.PolicyError = errors.New("policy unreadable")
	_, err := f.s.Decide(t.Context(), update())
	if err == nil {
		t.Fatal("unreadable policy accepted")
	}
}
func TestArchivedReceiveRefused(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.archiveRepository(t, repository)
	_, err := f.s.Decide(t.Context(), update())
	requireCode(t, err, publicerr.InvalidTransition)
}
func TestDecideUsesQuarantineIdentityWithoutReadingMovedRef(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.git.Resolve = func(context.Context, string, string) (gitport.Ref, error) {
		t.Fatal("gate read ordinary ref")
		return gitport.Ref{}, nil
	}
	d, err := f.s.Decide(t.Context(), update())
	if err != nil || d.Request.CommitSHA == nil || *d.Request.CommitSHA != head {
		t.Fatalf("%+v %v", d, err)
	}
}
func TestAcceptSealedWholePushOnce(t *testing.T) {
	t.Parallel()
	f := setup(t)
	b := batch(t)
	result, err := f.s.Accept(t.Context(), b)
	rows, fresh := result.Runs, result.Fresh
	if err != nil || !fresh || len(rows) != 1 || rows[0].Admission.Digest != b.Runs[0].Prepared.View().Digest {
		t.Fatalf("%+v %v %v", rows, fresh, err)
	}
	result, err = f.s.Accept(t.Context(), b)
	rows, fresh = result.Runs, result.Fresh
	if err != nil || fresh || len(rows) != 1 {
		t.Fatal("replay", err)
	}
	if len(f.store.native.Audits()) != 3 {
		t.Fatal("replayed audit")
	}
	page, err := f.s.EventsSince(t.Context(), caller(), eventsInput{})
	if err != nil || len(page.Events) != 2 {
		t.Fatalf("events %+v %v", page, err)
	}
}
func TestStorageValidatesAcceptanceBindingsAtomically(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"seal", "actor", "repo"} {
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			b := batch(t)
			switch name {
			case "seal":
				b.Runs[0].Prepared = nil
			case "actor":
				b.Runs[0].Request.Actor.Identity = "other"
			case "repo":
				b.Runs[0].Request.Repo = "hidden/team/widget"
			}
			_, err := f.s.Accept(t.Context(), b)
			requireCode(t, err, publicerr.InvalidInput)
			if f.store.Calls()["AcceptBatch"] != 1 || len(f.store.native.Audits()) != 0 {
				t.Fatal("store validation did not refuse atomically")
			}
		})
	}
}
func TestBatchRollbackOnBadEvent(t *testing.T) {
	t.Parallel()
	f := setup(t)
	b := batch(t)
	b.Events = []stateport.EventWrite{{Event: model.AgentEvent{Kind: "unknown"}, Dedupe: "bad"}}
	_, err := f.s.Accept(t.Context(), b)
	if err == nil {
		t.Fatal("invalid event accepted")
	}
	if len(f.store.native.Audits()) != 0 {
		t.Fatal("partial audit")
	}
	_, err = f.store.Run(t.Context(), runID)
	requireCode(t, err, publicerr.NotFound)
}
func TestReceiveReplayCannotChangeReceipt(t *testing.T) {
	t.Parallel()
	f := setup(t)
	b := batch(t)
	if _, err := f.s.Accept(t.Context(), b); err != nil {
		t.Fatal(err)
	}
	b.Runs[0].Request.ObjectSHA = strings.Repeat("b", 40)
	_, err := f.s.Accept(t.Context(), b)
	requireCode(t, err, publicerr.Conflict)
}
func TestStoppedServiceRefusesAcceptance(t *testing.T) {
	t.Parallel()
	f := setup(t)
	if err := f.s.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, err := f.s.Accept(t.Context(), batch(t))
	requireCode(t, err, publicerr.Unavailable)
	if f.store.Calls()["AcceptBatch"] != 0 {
		t.Fatal("accepted after shutdown")
	}
}
func TestMultiRefAcceptanceIsOneAtomicTransaction(t *testing.T) {
	t.Parallel()
	for _, badSecond := range []bool{false, true} {
		f := setup(t)
		b := batch(t)
		second := b.Runs[0]
		second.Request = second.Request.Clone()
		second.Request.Ref = "refs/heads/second"
		view := second.Prepared.View()
		view.RunID = "bbbbbbbb000000000000000000000002"
		view.Ref = second.Request.Ref
		prepared, err := admissioncontract.Seal(view)
		if err != nil {
			t.Fatal(err)
		}
		second.Prepared = prepared
		second.Events = nil
		if badSecond {
			second.Request.ObjectSHA = strings.Repeat("b", 40)
		}
		b.Runs = append(b.Runs, second)
		result, err := f.s.Accept(t.Context(), b)
		rows, fresh := result.Runs, result.Fresh
		if badSecond {
			if err == nil {
				t.Fatal("bad second ref accepted")
			}
			_, err = f.store.Run(t.Context(), runID)
			requireCode(t, err, publicerr.NotFound)
		} else if err != nil || !fresh || len(rows) != 2 || f.store.Calls()["AcceptBatch"] != 1 {
			t.Fatalf("batch %+v %v %v", rows, fresh, err)
		}
	}
}
func TestAcceptanceSealBindsTriggerAndFullActor(t *testing.T) {
	t.Parallel()
	for _, field := range []string{"trigger", "actor"} {
		f := setup(t)
		b := batch(t)
		if field == "trigger" {
			b.Runs[0].Request.Trigger = model.TriggerTag
		} else {
			b.Runs[0].Request.Actor.Fingerprint = "other"
		}
		_, err := f.s.Accept(t.Context(), b)
		if err == nil {
			t.Fatalf("%s binding changed", field)
		}
		if len(f.store.native.Audits()) != 0 {
			t.Fatal("partial acceptance audit")
		}
	}
}
