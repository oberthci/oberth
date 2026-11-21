package app_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func TestAssistPushRunsWithoutAssistAndWarnsOnce(t *testing.T) {
	t.Parallel()
	for _, gated := range []bool{true, false} {
		t.Run(map[bool]string{true: "gated", false: "receive_replay"}[gated], func(t *testing.T) { checkAssistPush(t, gated) })
	}
}
func checkAssistPush(t *testing.T, gated bool) {
	t.Helper()
	f := newFixture(t)
	preparations := expectAssistOff(t, f)
	s := f.new(t)
	r := receive()
	r.Source = f.source
	r.Updates[0].Request.Assist = true
	other := r.Updates[0]
	other.Request.RunID, other.Request.Ref.Name = "r2", "refs/heads/other"
	r.Updates = append(r.Updates, other)
	if gated {
		if _, err := s.Gate(t.Context(), r); err != nil {
			t.Fatal("assist option refused the push", err)
		}
	}
	for range 2 {
		result, err := s.Deliver(t.Context(), r)
		assertUnassistedPushStarted(t, result, err)
	}
	if *preparations != 2 || f.prepares.Load() != 2 || f.creates.Load() != 2 {
		t.Fatal("replay repeated preparation or native creation")
	}
	if !r.Updates[0].Request.Assist || !r.Updates[1].Request.Assist {
		t.Fatal("push caller's option was mutated")
	}
}
func assertUnassistedPushStarted(t *testing.T, result app.PushResult, err error) {
	t.Helper()
	if err != nil || len(result.Outcomes) != 2 {
		t.Fatal("assist option prevented submission", result, err)
	}
	assertAssistWarning(t, result, 1)
	for _, outcome := range result.Outcomes {
		if outcome.State != "started" {
			t.Fatal("run did not start", outcome)
		}
	}
}

func TestPushWithoutAssistHasNoAssistWarning(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	preparations := expectAssistOff(t, f)
	s := f.new(t)
	r := f.gate(t, s)
	result, err := s.Deliver(t.Context(), r)
	if err != nil || f.creates.Load() != 1 || *preparations != 1 {
		t.Fatal("ordinary push did not run", result, err)
	}
	assertAssistWarning(t, result, 0)
}

func TestManualAndInspectPreparationCannotEnableAssist(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	preparations := expectAssistOff(t, f)
	s := f.new(t)
	r := request()
	r.Assist = true
	if _, err := s.Check(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	q := startRequest("assist-manual", r)
	q.Request.Assist = true
	result, err := s.StartRun(t.Context(), q)
	if err != nil || result.State != "started" || *preparations != 2 {
		t.Fatal(result, err)
	}
}

func expectAssistOff(t *testing.T, f *fixture) *int {
	t.Helper()
	f.d.Preparation.Assist = true
	load := f.authority.LoadFunc
	f.authority.LoadFunc = func(ctx context.Context, scope app.Scope) (admissioncontract.PrepareRequest, argoworkflow.Policy, error) {
		if scope.Assist {
			t.Error("authority received enabled assist")
		}
		return load(ctx, scope)
	}
	calls := new(int)
	hooks := f.native.Hook
	f.native.HookFunc = func(p nativebinding.Preparation) admissioncontract.PrepareHooks {
		*calls++
		if p.Assist {
			t.Error("native preparation received enabled assist")
		}
		return hooks
	}
	return calls
}

func assertAssistWarning(t *testing.T, result app.PushResult, want int) {
	t.Helper()
	count := 0
	for _, line := range result.Lines() {
		if line == "assist is not available in this version; running without it" {
			count++
		}
	}
	if count != want {
		t.Fatal("incorrect assist warning count", result.Lines(), want)
	}
}

func TestReviewDisabledAssistHasOrdinaryEvidence(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	expectAssistOff(t, f)
	s := f.new(t)
	r := receive()
	r.Source = f.source
	r.Updates[0].Request.Assist = true
	result, err := s.Deliver(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	assertAssistWarning(t, result, 1)
	row, err := f.runs.Run(t.Context(), r.Updates[0].Request.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Origin.HasAssistance() {
		t.Error("disabled assist still disqualifies ordinary CI evidence", row.Origin)
	}
	err = f.runs.Finish(t.Context(), stateport.Action{Actor: row.Origin.Actor}, stateport.RunUpdate{ID: row.ID, Revision: row.Revision, Status: stepmodel.RunPassed, ProjectCI: true})
	if err != nil {
		t.Error("passing run promised ordinary execution cannot project CI", err)
	}
}

func TestAssistRerunPreservesSubmissionWarning(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	expectAssistOff(t, f)
	f.keeper.KeepFunc = func(context.Context, string, []sourcecontract.Binding) error { return errors.New("source unavailable") }
	q := startRequest("assist-unavailable", request())
	q.Request.Assist = true
	result, err := f.new(t).StartRun(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(result.Warning, "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "stays accepted") || lines[1] != "assist is not available in this version; running without it" {
		t.Fatal("warning was replaced or repeated", result.Warning)
	}
}
func TestOrdinaryRerunHasNoAssistNotice(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	result, err := f.new(t).StartRun(t.Context(), startRequest("ordinary-rerun", request()))
	if err != nil || result.Warning != "" {
		t.Fatal("ordinary rerun printed assist warning", result, err)
	}
}
