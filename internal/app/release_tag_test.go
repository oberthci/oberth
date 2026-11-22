package app_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
)

// tagForge records each delivery with what existed when it happened.
type tagForge struct {
	refuse      error
	delivered   []gitport.Ref
	runsAtThen  []bool
	createsThen []int32
	f           *fixture
	receive     string
}

func (g *tagForge) DeliverTag(ctx context.Context, ref gitport.Ref) error {
	_, err := g.f.runs.AcceptedRuns(ctx, g.receive)
	g.delivered = append(g.delivered, ref)
	g.runsAtThen = append(g.runsAtThen, err == nil)
	g.createsThen = append(g.createsThen, g.f.creates.Load())
	return g.refuse
}

func releaseFixture(t *testing.T) (*fixture, app.Receive) {
	t.Helper()
	f := newFixture(t)
	f.runs.DecideFunc = func(_ context.Context, u service.RefUpdate) (service.Decision, error) {
		d := decision(u, "")
		d.Release = true
		return d, nil
	}
	f.d.ReleaseFacts = func(_ context.Context, r app.Request, _ string) (admissioncontract.ReleaseFacts, error) {
		return admissioncontract.ReleaseFacts{Version: "v1.2.3", TestedCommit: r.Ref.CommitSHA, DefaultCommit: r.Ref.CommitSHA, BuildStatus: "passed"}, nil
	}
	r := receive()
	r.Source = f.source
	u := &r.Updates[0].Request
	u.Release, u.Trigger, u.Ref.Name = true, model.TriggerTag, "refs/tags/v1.2.3"
	return f, r
}

func TestGateDeliversReleaseTagBeforeAnyRunExists(t *testing.T) {
	t.Parallel()
	f, r := releaseFixture(t)
	forge := &tagForge{f: f, receive: r.ID}
	r.Forge = forge
	s := f.new(t)
	if _, err := s.Gate(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	want := gitport.Ref(r.Updates[0].Request.Ref)
	if len(forge.delivered) != 1 || forge.delivered[0] != want {
		t.Fatalf("delivered %+v, want exactly %+v", forge.delivered, want)
	}
	if forge.runsAtThen[0] || forge.createsThen[0] != 0 {
		t.Fatal("a run existed before the tag reached the forge")
	}
	if _, err := s.Deliver(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if !saved(f, r.ID) || f.creates.Load() != 1 || len(forge.delivered) != 1 {
		t.Fatal("the delivered release did not run exactly once after delivery")
	}
}

func TestGateRefusesReleaseWhoseTagIsNotDelivered(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		refuse error
		want   string
	}{
		"forge refuses": {&gitport.AlreadyExistsError{Ref: "refs/tags/v1.2.3"}, "upstream tag already exists"},
		"no forge":      {nil, "release tag not delivered to the forge"},
	} {
		t.Run(name, func(t *testing.T) {
			f, r := releaseFixture(t)
			if tc.refuse != nil {
				r.Forge = &tagForge{refuse: tc.refuse, f: f, receive: r.ID}
			}
			_, err := f.new(t).Gate(context.Background(), r)
			assertNoRunAfterRefusal(t, f, err, tc.want)
		})
	}
}

func assertNoRunAfterRefusal(t *testing.T, f *fixture, err error, cause string) {
	t.Helper()
	var refused *app.PushRefusal
	if !errors.As(err, &refused) {
		t.Fatalf("undelivered tag admitted: %v", err)
	}
	for _, want := range []string{"push refused; nothing ran", "refs/tags/v1.2.3", cause} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal does not say %q: %s", want, err)
		}
	}
	if saved(f, receive().ID) || f.creates.Load() != 0 {
		t.Fatal("refused delivery left a run")
	}
}

func TestGateDeliversNoTagForBranchesOrRefusedPushes(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	forge := &tagForge{f: f}
	r := receive()
	r.Source, r.Forge = f.source, forge
	if _, err := f.new(t).Gate(context.Background(), r); err != nil || len(forge.delivered) != 0 {
		t.Fatal("a branch push reached the forge", err)
	}
	g, refused := releaseFixture(t)
	g.source.Read = missingFile
	refused.Source, refused.Forge = g.source, &tagForge{f: g, receive: refused.ID}
	if _, err := g.new(t).Gate(context.Background(), refused); err == nil || len(refused.Forge.(*tagForge).delivered) != 0 {
		t.Fatal("a refused release reached the forge", err)
	}
}

func TestGateRefusesMoreThanOneReleaseTagBeforeAnyDelivery(t *testing.T) {
	t.Parallel()
	f, r := releaseFixture(t)
	second := r.Updates[0]
	second.Request.RunID, second.Request.Ref.Name, second.Request.Trigger = "r2", "refs/tags/v1.2.4", model.TriggerTag
	r.Updates = append(r.Updates, second)
	forge := &tagForge{f: f, receive: r.ID}
	r.Forge = forge
	_, err := f.new(t).Gate(context.Background(), r)
	if err == nil || !strings.Contains(err.Error(), "push carries more than one release tag") || !strings.Contains(err.Error(), "refs/tags/v1.2.4") {
		t.Fatal("several release tags in one push were not refused", err)
	}
	if len(forge.delivered) != 0 || saved(f, r.ID) || f.creates.Load() != 0 {
		t.Fatal("a refused multi-release push reached the forge or left a run")
	}
}
