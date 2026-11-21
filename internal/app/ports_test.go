package app_test

import (
	"context"
	"errors"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/app/contracttest"
	"github.com/oberthci/oberth/internal/app/fake"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"golang.org/x/crypto/ssh"
)

func TestConsumerPortContracts(t *testing.T) {
	t.Parallel()
	contracttest.Decider(t, func(*testing.T) contracttest.DeciderFixture {
		f := newFixture(t)
		return contracttest.DeciderFixture{Port: fake.Service{DecideFunc: f.runs.Decide}, Update: service.RefUpdate{ID: "receive1", Repo: request().Repository, Ref: request().Ref.Name, ObjectSHA: request().Ref.ObjectSHA, CommitSHA: request().Ref.CommitSHA, Actor: request().Actor}, Decision: decision(service.RefUpdate{ID: "receive1", Repo: request().Repository, Ref: request().Ref.Name, ObjectSHA: request().Ref.ObjectSHA, CommitSHA: request().Ref.CommitSHA, Actor: request().Actor}, "")}
	})
	contracttest.Keeper(t, func(t *testing.T) contracttest.KeeperFixture {
		f := newFixture(t)
		w := workflow()
		addSource(w, sourcecontract.Descriptor{Version: 1, Kind: sourcecontract.Main})
		f.setDocument(w)
		p := mustAdmit(t, f.new(t), f)
		k := &fake.Keeper{}
		return contracttest.KeeperFixture{Port: k, Bindings: k.Bindings, Binding: p.View().SourceBindings[0]}
	})
	contracttest.Source(t, sourceFixture)
	contracttest.Authority(t, authorityFixture)
	contracttest.Runs(t, func(t *testing.T) contracttest.RunsFixture {
		f := newFixture(t)
		p := mustAdmit(t, f.new(t), f)
		return contracttest.RunsFixture{Port: statePortFake{Runs: fake.Runs{Receipts: f.runs.AcceptedRuns, Stops: f.runs.Obligations, Fail: f.runs.FailUnbound, Unbound: f.runs.UnboundRuns}, Service: fake.Service{DecideFunc: f.runs.Decide, AcceptFunc: f.runs.Accept}}, DuplicateRoot: duplicateRoot(t, p), Acceptance: batch(p, "ports"), Stop: func(value string) {
			mode := "Stop"
			if value == "Terminate" {
				mode = "Terminate"
			}
			f.runs.SetStop(t.Context(), "r1", mode)
		}}
	})
	contracttest.Native(t, func(t *testing.T) contracttest.NativeFixture {
		f := newFixture(t)
		p := mustAdmit(t, f.new(t), f)
		recorded, err := f.runs.AcceptBatch(t.Context(), batch(p, "native"))
		if err != nil {
			t.Fatal(err)
		}
		return contracttest.NativeFixture{Port: fake.NewNative(f.runs.Run, f.bindNative), Prepared: p, Run: recorded.Runs[0], Stored: f.runs.Run}
	})
	contracttest.HostScanner(t, func(t *testing.T) contracttest.ScannerFixture {
		key := hostKey(t, 1)
		s := fake.Scanner{ScanFunc: func(ctx context.Context, _, _ string) (ssh.PublicKey, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return key, nil
		}}
		return contracttest.ScannerFixture{Port: s, Address: "forge:22", Algorithm: ssh.KeyAlgoED25519, Fingerprint: ssh.FingerprintSHA256(key)}
	})

}
func sourceFixture(t *testing.T) contracttest.SourceFixture {
	t.Helper()
	f := newFixture(t)
	base := f.source.Read
	f.source.Read = func(ctx context.Context, r gitport.Revision, path string) (gitport.File, error) {
		if path == ".oberth/missing.yaml" {
			return missingFile(ctx, r, path)
		}
		return base(ctx, r, path)
	}
	first := sourcecontract.Revision{Repository: request().Repository, Commit: request().Ref.CommitSHA}
	second := first
	second.Commit = strings.Repeat("d", 40)
	return contracttest.SourceFixture{Port: f.source, First: first, Second: second, File: ".oberth/build.yaml", FirstBytes: sourceBytes(t, f, first), SecondBytes: sourceBytes(t, f, second), Ref: request().Ref, ProducerID: 2}
}
func authorityFixture(t *testing.T) contracttest.AuthorityFixture {
	t.Helper()
	f := newFixture(t)
	return contracttest.AuthorityFixture{Port: f.authority, Scope: app.Scope{RepositoryID: 1, Preparation: nativebinding.Preparation{Repository: request().Repository, File: ".oberth/release.yaml", Release: true}}}
}
func TestEveryConstructorDependencyIsRequired(t *testing.T) {
	t.Parallel()
	for _, field := range []string{"source", "authority", "runs", "native", "keeper", "deadline", "url", "lifetime", "service", "resolver"} {
		t.Run(field, func(t *testing.T) {
			f := newFixture(t)
			switch field {
			case "service":
				f.d.Service = nil
			case "resolver":
				f.d.Resolver = nil
			case "source":
				f.d.Source = nil
			case "authority":
				f.d.Authority = nil
			case "runs":
				f.d.Runs = nil
			case "native":
				f.d.Native = nil
			case "keeper":
				f.d.Keeper = nil
			case "deadline":
				f.d.Deadline = nil
			case "lifetime":
				f.d.Lifetime = nil
			case "url":
				f.d.PublicURL = "invalid"
			}
			_, err := app.New(f.d)
			expectRefusal(t, err, "config")
		})
	}
}
func TestClosedSubmitterRejectsNewWork(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	s := f.new(t)
	closeSubmitter(t, s)
	_, err := s.StartRun(context.Background(), startRequest("closed", request()))
	expectRefusal(t, err, "closed")
	r := receive()
	r.Source = f.source
	_, err = s.Gate(context.Background(), r)
	expectRefusal(t, err, "closed")
	if f.creates.Load() != 0 {
		t.Fatal("closed server created work")
	}
}
func TestAcceptanceFailureCanRetryWithoutPreparingAgain(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	s := f.new(t)
	r := f.gate(t, s)
	fail := true
	f.runs.BeforeAccept = func(context.Context, stateport.BatchAcceptance) error {
		if fail {
			return errors.New("acceptance unavailable")
		}
		return nil
	}
	if _, err := s.Deliver(context.Background(), r); err == nil {
		t.Fatal("acceptance failure hidden")
	}
	fail = false
	if _, err := s.Deliver(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if f.prepares.Load() != 1 || f.creates.Load() != 1 {
		t.Fatal("retry did not reuse seal")
	}
}
func TestUnknownAuthorityNeverLoadsUnscoped(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	r := request()
	r.Actor = actor.Actor{}
	_, err := f.new(t).Admit(context.Background(), r, f.source)
	expectRefusal(t, err, "request")
}

func TestReceiveResultsCannotMutateAcceptance(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	s := f.new(t)
	r := receive()
	r.Source = f.source
	result, err := s.Gate(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	result.Outcomes[0].RunID = "mutated"
	result, err = s.Deliver(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcomes[0].RunID != "r1" {
		t.Fatal("gate result aliases acceptance")
	}
	result.Outcomes[0].RunID = "mutated"
	again, err := s.Deliver(context.Background(), r)
	if err != nil || again.Outcomes[0].RunID != "r1" {
		t.Fatal("delivery result aliases receipt", again, err)
	}
	if !strings.Contains(strings.Join(again.Lines(), "\n"), "https://oberth.ci/runs/r1") {
		t.Fatal("missing watch link", again.Lines())
	}
}
func TestManualSubmissionFinishesAfterAcceptanceCancellation(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.keeper.KeepFunc = func(ctx context.Context, _ string, _ []sourcecontract.Binding) error { cancel(); return ctx.Err() }
	s := f.new(t)
	if _, err := s.StartRun(ctx, startRequest("manual", request())); err != nil {
		t.Fatal(err)
	}
	closeSubmitter(t, s)
	if _, ok := f.runs.Workflow(t.Context(), "r1"); !ok {
		t.Fatal("accepted manual submission was cancelled")
	}
}
func TestReceiveIdentityIsRequired(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	s := f.new(t)
	r := receive()
	r.ID = ""
	r.Source = f.source
	_, err := s.Gate(context.Background(), r)
	expectRefusal(t, err, "request")
	_, err = s.Deliver(context.Background(), r)
	expectRefusal(t, err, "request")

}

func closeSubmitter(t *testing.T, s *app.Submitter) {
	t.Helper()
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func sourceBytes(t *testing.T, f *fixture, r sourcecontract.Revision) []byte {
	t.Helper()
	got, err := f.source.ReadFile(context.Background(), r, ".oberth/build.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return got.Bytes
}
func TestPublicURLRequiresHTTPHost(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"https://oberth.ci", "http://oberth.ci/runs"} {
		f := newFixture(t)
		f.d.PublicURL = raw
		s, err := app.New(f.d)
		if err != nil {
			t.Fatal(raw, err)
		}
		closeSubmitter(t, s)
	}
	for _, raw := range []string{"", "oberth.ci", "/runs", "https:///runs", "ssh://oberth.ci", "%broken"} {
		f := newFixture(t)
		f.d.PublicURL = raw
		_, err := app.New(f.d)
		expectRefusal(t, err, "config")
	}
}
func TestSubmitHonorsFreshAcceptance(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.runs.BeforeAccept = func(ctx context.Context, a stateport.BatchAcceptance) error {
		f.runs.BeforeAccept = nil
		_, err := f.runs.AcceptBatch(ctx, a)
		return err
	}
	if _, err := f.new(t).StartRun(context.Background(), startRequest("already-accepted", request())); err != nil {
		t.Fatal(err)
	}
	if f.creates.Load() != 0 {
		t.Fatal("nonfresh acceptance submitted again")
	}
}

// The callback fakes delegate persistence to the owner; neither holds store facts.
type statePortFake struct {
	fake.Runs
	fake.Service
}
