package app_test

import (
	"context"
	"errors"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/sourcecontract"
)

func TestOnePreparePerRun(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	s := f.new(t)
	r := f.gate(t, s)
	if _, err := s.Deliver(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	s = f.new(t)
	if err := s.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.prepares.Load() != 1 || f.creates.Load() != 1 {
		t.Fatal("repeated admission or creation")
	}
}
func TestGateRefusesWholePushListingEveryFinding(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	w := workflow()
	w.Spec.Templates[0].Container.Image = "unapproved/image:1"
	w.Spec.Templates[0].PodSpecPatch = "{}"
	f.setDocument(w)
	s := f.new(t)
	r := receive()
	u := r.Updates[0]
	u.Request.RunID = "r2"
	u.Request.Ref.Name = "refs/heads/other"
	r.Updates = append(r.Updates, u)
	r.Source = f.source
	_, err := s.Gate(context.Background(), r)
	var e *app.PushRefusal
	if !errors.As(err, &e) || len(e.Problems) != 2 {
		t.Fatal(err)
	}
	text := e.Error()
	if !strings.Contains(text, "refs/heads/other") || !strings.Contains(text, "refs/heads/topic") || !strings.Contains(text, ".oberth/build.yaml") || !strings.Contains(text, "container.image") || !strings.Contains(text, "podSpecPatch") {
		t.Fatal(text)
	}
	if saved(f, r.ID) || f.creates.Load() != 0 {
		t.Fatal("refused push caused effects")
	}
}
func TestGateServiceRefusalRejectsEveryRefAndShowsCause(t *testing.T) {
	t.Parallel()
	for _, permanent := range []bool{true, false} {
		t.Run(map[bool]string{true: "refusal", false: "outage"}[permanent], func(t *testing.T) {
			f := newFixture(t)
			kind := publicerr.Forbidden
			reason := "main requires an exact tested commit"
			remedy := "Promote the tested commit."
			if !permanent {
				kind = publicerr.Unavailable
				reason = "repository storage is offline"
				remedy = "Try again when storage is available."
			}
			f.runs.DecideFunc = func(_ context.Context, u service.RefUpdate) (service.Decision, error) {
				if u.Ref == "refs/heads/topic" {
					return service.Decision{}, publicerr.New(kind, reason, remedy, nil)
				}
				return decision(u, ""), nil
			}
			s := f.new(t)
			r := receive()
			u := r.Updates[0]
			u.Request.RunID = "r2"
			u.Request.Ref.Name = "refs/heads/other"
			r.Updates = append(r.Updates, u)
			r.Source = f.source
			_, err := s.Gate(context.Background(), r)
			if err == nil {
				t.Fatal("service refusal vanished")
			}
			text := err.Error()
			if !strings.Contains(text, reason) || !strings.Contains(text, remedy) || !strings.Contains(text, "refs/heads/topic") {
				t.Fatal(text)
			}
			if saved(f, r.ID) || f.creates.Load() != 0 {
				t.Fatal("bad ref did not reject the push")
			}
		})
	}
}
func TestGateSkipsParkedDeletedAndNoPipeline(t *testing.T) {
	t.Parallel()
	for _, skip := range []string{"parked", "deleted", ""} {
		t.Run(map[string]string{"": "no_pipeline", "parked": "parked", "deleted": "deleted"}[skip], func(t *testing.T) {
			f := newFixture(t)
			reads := 0
			f.source.Read = func(ctx context.Context, r gitport.Revision, path string) (gitport.File, error) {
				reads++
				return missingFile(ctx, r, path)
			}
			f.runs.DecideFunc = func(_ context.Context, u service.RefUpdate) (service.Decision, error) {
				return decision(u, skip), nil
			}
			s := f.new(t)
			r := receive()
			r.Source = f.source
			result, err := s.Gate(context.Background(), r)
			if err != nil {
				t.Fatal(err)
			}
			if f.prepares.Load() != 0 || len(result.Outcomes) != 1 {
				t.Fatal(result)
			}
			if skip != "" && reads != 0 {
				t.Fatal("skipped ref read YAML")
			}
			if skip == "parked" && !strings.Contains(strings.Join(result.Lines(), "\n"), "-o oberth.run=1") {
				t.Fatal(result.Lines())
			}
		})
	}
}
func TestGateMissingReleaseRefuses(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.runs.DecideFunc = func(_ context.Context, u service.RefUpdate) (service.Decision, error) {
		d := decision(u, "")
		d.Release = true
		return d, nil
	}
	f.source.Read = missingFile
	s := f.new(t)
	r := receive()
	r.Source = f.source
	r.Updates[0].Request.Release = true
	r.Updates[0].Request.Trigger = "tag"
	r.Updates[0].Request.Ref.Name = "refs/tags/v1.0.0"
	if _, err := s.Gate(context.Background(), r); err == nil {
		t.Fatal("missing release accepted")
	}
}
func TestDeliverAcceptsOncePerReceive(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	s := f.new(t)
	r := f.gate(t, s)
	for range 2 {
		if _, err := s.Deliver(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	}
	if f.prepares.Load() != 1 || f.creates.Load() != 1 {
		t.Fatal("receive replayed")
	}
}
func TestDeliverAfterCrashAdmitsOnce(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	s := f.new(t)
	r := receive()
	for range 2 {
		if _, err := s.Deliver(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	}
	if f.prepares.Load() != 1 || f.creates.Load() != 1 {
		t.Fatal("crash delivery replayed")
	}
}
func TestDeliverAfterCrashRecordsAuthoredRefusal(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	w := workflow()
	w.Spec.Templates[0].Container.Image = "unapproved/image:1"
	f.setDocument(w)
	s := f.new(t)
	result, err := s.Deliver(context.Background(), receive())
	if err != nil {
		t.Fatal(err)
	}
	failure, ok := f.runs.Failure(t.Context(), "r1")
	if !ok || !strings.Contains(strings.Join(result.Lines(), "\n"), "allowed registries") || f.creates.Load() != 0 || result.Outcomes[0].State != "failed" {
		t.Fatal(failure, result, ok)
	}
}
func TestReplayPermanentServiceRefusalIsRecordedAndDoesNotBlockNextPush(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.runs.DecideFunc = func(_ context.Context, u service.RefUpdate) (service.Decision, error) {
		if u.ID != "next" {
			return service.Decision{}, publicerr.New(publicerr.Forbidden, "tag is outside default history", "Use a tag on the default branch history.", nil)
		}
		return decision(u, ""), nil
	}
	s := f.new(t)
	first := receive()
	for range 2 {
		result, err := s.Deliver(context.Background(), first)
		if err != nil || result.Outcomes[0].State != "failed" {
			t.Fatal(result, err)
		}
		if !strings.Contains(strings.Join(result.Lines(), "\n"), "tag is outside default history") {
			t.Fatal(result.Lines())
		}
	}
	if !saved(f, first.ID) {
		t.Fatal("refusal was not recorded")
	}
	next := receive()
	next.ID = "next"
	next.Updates[0].Request.RunID = "r2"
	if _, err := s.Deliver(context.Background(), next); err != nil {
		t.Fatal("earlier refusal blocked later push", err)
	}
	if _, ok := f.runs.Workflow(t.Context(), "r2"); !ok {
		t.Fatal("later push did not submit")
	}
}
func TestReplayOutageIsRetriedWithoutFailedRun(t *testing.T) {
	t.Parallel()
	for _, stage := range []string{"decide", "admission"} {
		t.Run(stage, func(t *testing.T) { runReplayOutage(t, stage) })
	}
}
func TestDeliverHonorsFreshAcceptance(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.runs.BeforeAccept = func(ctx context.Context, a stateport.BatchAcceptance) error {
		f.runs.BeforeAccept = nil
		_, err := f.runs.AcceptBatch(ctx, a)
		return err
	}
	s := f.new(t)
	r := f.gate(t, s)
	if _, err := s.Deliver(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if f.creates.Load() != 0 {
		t.Fatal("nonfresh acceptance submitted")
	}
}
func TestStartFailureIsNeverReportedAsStarted(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.keeper.KeepFunc = func(context.Context, string, []sourcecontract.Binding) error { return errors.New("keep failed") }
	s := f.new(t)
	r := f.gate(t, s)
	result, err := s.Deliver(context.Background(), r)
	if err != nil || result.Outcomes[0].State != "accepted" || !strings.Contains(strings.Join(result.Lines(), "\n"), "keep failed") {
		t.Fatal(result, err)
	}
}
func TestCompletedDeliveryDropsItsStash(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	s := f.new(t)
	r := f.gate(t, s)
	if _, err := s.Deliver(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if app.ReceiveStashSize(s) != 0 {
		t.Fatal("completed delivery retained its stash")
	}
	if _, err := s.Deliver(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if f.prepares.Load() != 1 || f.creates.Load() != 1 {
		t.Fatal("durable replay prepared again")
	}
}

func runReplayOutage(t *testing.T, stage string) {
	t.Helper()
	f := newFixture(t)
	offline := true
	outage := publicerr.New(publicerr.Unavailable, "authority storage is offline", "Try again when storage recovers.", nil)
	if stage == "decide" {
		f.runs.DecideFunc = func(_ context.Context, u service.RefUpdate) (service.Decision, error) {
			if offline {
				return service.Decision{}, outage
			}
			return decision(u, ""), nil
		}
	} else {
		read := f.source.Read
		f.source.Read = func(ctx context.Context, r gitport.Revision, path string) (gitport.File, error) {
			if offline {
				return gitport.File{}, outage
			}
			return read(ctx, r, path)
		}
	}
	s := f.new(t)
	r := receive()
	if _, err := s.Deliver(context.Background(), r); err == nil {
		t.Fatal("outage accepted")
	}
	if _, failed := f.runs.Failure(t.Context(), "r1"); failed {
		t.Fatal("outage became permanent failure")
	}
	if saved(f, r.ID) {
		t.Fatal("outage consumed the receive")
	}
	offline = false
	if _, err := s.Deliver(context.Background(), r); err != nil {
		t.Fatal("outage was not retried", err)
	}
	if _, bound := f.runs.Workflow(t.Context(), "r1"); !bound {
		t.Fatal("replay after outage did not submit")
	}
}

func TestAcceptedRunsReadFailureNeverReplaysAcceptance(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	s := f.new(t)
	r := f.gate(t, s)
	outage := errors.New("receipt reader unavailable")
	f.runs.BeforeAccepted = func(context.Context, string) error { return outage }
	if _, err := s.Deliver(context.Background(), r); !errors.Is(err, outage) {
		t.Fatal("receipt read outage hidden", err)
	}
	if saved(f, r.ID) || f.creates.Load() != 0 {
		t.Fatal("receipt read failure became absence")
	}
	f.runs.BeforeAccepted = nil
	if _, err := s.Deliver(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if f.prepares.Load() != 1 || f.creates.Load() != 1 {
		t.Fatal("retry did not reuse the original seal")
	}
}
func TestPushFixLineSaysTheAdministratorRunsTheCommand(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	command := publicerr.UnarchiveCommand("origin/team/widget")
	f.runs.DecideFunc = func(context.Context, service.RefUpdate) (service.Decision, error) {
		return service.Decision{}, publicerr.New(publicerr.InvalidTransition, "The repository is archived.", "Ask an administrator to unarchive the repository.", nil).WithCommand(command)
	}
	s := f.new(t)
	r := receive()
	r.Source = f.source
	_, err := s.Gate(context.Background(), r)
	if err == nil || !strings.Contains(err.Error(), "\n  fix: Administrator: "+command) || strings.Contains(err.Error(), "fix: "+command) {
		t.Fatal("the fix line does not say who runs the command", err)
	}
}

func TestServiceRefusalLeadsWithItsReasonAndRemedy(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.runs.DecideFunc = func(context.Context, service.RefUpdate) (service.Decision, error) {
		return service.Decision{}, publicerr.New(publicerr.Forbidden, "key is outside repository scope", "Use a key scoped to this repository.", nil)
	}
	s := f.new(t)
	r := receive()
	r.Source = f.source
	_, err := s.Gate(context.Background(), r)
	want := "refs/heads/topic: key is outside repository scope.\n  fix: Use a key scoped to this repository."
	if err == nil || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), "ref was refused") {
		t.Fatal(err)
	}
}
