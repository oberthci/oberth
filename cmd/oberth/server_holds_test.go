package main

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

type holdNativeProbe struct {
	t                  *testing.T
	run                stateport.Run
	binding            nativebinding.RunBinding
	hold               model.LiveHold
	request            argojob.HoldExecRequest
	result             argojob.ExecResult
	found              bool
	findErr, nativeErr error
	effects            int
}

func newHoldProbe(t *testing.T) *holdNativeProbe {
	return &holdNativeProbe{t: t, found: true,
		run:     stateport.Run{RunSummary: stepmodel.RunSummary{ID: "admitted-run"}},
		binding: nativebinding.RunBinding{Workflow: nativebinding.Object{Namespace: "runs", Name: "native-workflow", UID: "workflow-uid"}, RepositoryID: 7, RunID: "admitted-run", Revision: sourcecontract.Revision{Repository: "team/widget", Commit: "exact-commit"}, Ref: "refs/heads/topic", Trigger: "branch", Identity: "sealed-identity", SpecSHA256: "sealed-spec"},
		hold:    model.LiveHold{HeldNotice: model.HeldNotice{RunID: "admitted-run", Step: "build", NodeID: "selected-node", PodUID: "selected-pod-uid", HeldSince: time.Date(2026, 11, 18, 1, 0, 0, 0, time.UTC), TTLSeconds: 300}, PodName: "selected-pod", RemainingMS: 1500, AvailabilityUnknown: true},
		result:  argojob.ExecResult{Stdout: "out", Stderr: "detail", ExitCode: 23, Truncated: true},
	}
}
func (p *holdNativeProbe) Find(_ context.Context, r stateport.Run) (nativebinding.RunBinding, bool, error) {
	if !reflect.DeepEqual(r, p.run) {
		p.t.Fatal("run changed before native binding lookup")
	}
	return p.binding, p.found, p.findErr
}
func (p *holdNativeProbe) Holds(_ context.Context, b nativebinding.RunBinding) ([]model.LiveHold, error) {
	p.check(b, p.hold)
	p.effects++
	return []model.LiveHold{p.hold}, p.nativeErr
}
func (p *holdNativeProbe) Abandon(_ context.Context, b nativebinding.RunBinding, h model.LiveHold) error {
	p.check(b, h)
	p.effects++
	return p.nativeErr
}
func (p *holdNativeProbe) HoldExec(_ context.Context, q argojob.HoldExecRequest) (argojob.ExecResult, error) {
	p.check(q.Binding, q.Hold)
	p.effects++
	p.request = q
	return p.result, p.nativeErr
}
func (p *holdNativeProbe) check(b nativebinding.RunBinding, h model.LiveHold) {
	p.t.Helper()
	if b != p.binding || h != p.hold {
		p.t.Fatal("durable binding or selected live hold changed", b, h)
	}
}
func TestHoldNativeAdapterPreservesSelectionAndResult(t *testing.T) {
	fault := errors.New("native refusal")
	for _, err := range []error{nil, fault} {
		p := newHoldProbe(t)
		p.nativeErr = err
		h := heldRuns{p}
		rows, got := h.List(t.Context(), p.run)
		if !errors.Is(got, err) || len(rows) != 1 || rows[0] != p.hold {
			t.Fatal("native list changed", rows, got)
		}
		q := apicontract.RunHoldExecArgs{ID: p.run.ID, Step: "different-selector", Argv: []string{"printf", "value with spaces"}, TimeoutS: 37}
		result, got := h.Exec(t.Context(), p.run, p.hold, q)
		want := apicontract.ExecResult{Stdout: p.result.Stdout, Stderr: p.result.Stderr, ExitCode: 23, Truncated: true}
		if !errors.Is(got, err) || result != want {
			t.Fatal("native result changed", result, got)
		}
		if !reflect.DeepEqual(p.request.Command, q.Argv) || p.request.Timeout != 37*time.Second {
			t.Fatal("argv or timeout changed", p.request)
		}
		if got = h.Abandon(t.Context(), p.run, p.hold); !errors.Is(got, err) || p.effects != 3 {
			t.Fatal("release did not use selected native hold", got, p.effects)
		}
	}
}
func TestHoldNativeAdapterRefusesMissingBinding(t *testing.T) {
	for _, fault := range []error{nil, errors.New("lookup failed")} {
		p := newHoldProbe(t)
		p.found = false
		p.findErr = fault
		h := heldRuns{p}
		_, listErr := h.List(t.Context(), p.run)
		_, execErr := h.Exec(t.Context(), p.run, p.hold, apicontract.RunHoldExecArgs{Argv: []string{"true"}})
		releaseErr := h.Abandon(t.Context(), p.run, p.hold)
		for _, err := range []error{listErr, execErr, releaseErr} {
			if err == nil || fault != nil && !errors.Is(err, fault) {
				t.Fatal("missing binding was not refused", err)
			}
		}
		if p.effects != 0 {
			t.Fatal("unbound hold reached native effects")
		}
	}
}
