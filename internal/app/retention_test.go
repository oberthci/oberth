package app_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestOnlyPermanentRetentionRefusalsFailAcceptedRuns(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"unknown", "unavailable", "permanent"} {
		t.Run(kind, func(t *testing.T) { checkRetentionFailure(t, kind) })
	}
}
func checkRetentionFailure(t *testing.T, kind string) {
	t.Helper()
	f := newFixture(t)
	f.keeper.KeepFunc = func(context.Context, string, []sourcecontract.Binding) error { return retentionFailure(kind) }
	f.native.FindFunc = func(ctx context.Context, _ stateport.Run) (nativebinding.RunBinding, bool, error) {
		return nativebinding.RunBinding{}, false, ctx.Err()
	}
	s := f.new(t)
	out, err := s.StartRun(t.Context(), startRequest("retention-"+kind, request()))
	if err != nil || f.creates.Load() != 0 {
		t.Fatal(out, err, f.creates.Load())
	}
	row, err := f.runs.Run(t.Context(), "r1")
	if err != nil {
		t.Fatal(err)
	}
	assertRetentionOutcome(t, kind, out, row)
	if kind != "permanent" {
		f.keeper.KeepFunc = nil
		s = f.new(t)
		if err := s.Recover(t.Context()); err != nil {
			t.Fatal(err)
		}
		row, err = f.runs.Run(t.Context(), "r1")
		if err != nil || row.DisplayReason != stateport.ReasonRestartAbsent || f.creates.Load() != 0 {
			t.Fatal("restart did not pick up the accepted run", row.DisplayReason, err)
		}
	}
}
func assertRetentionOutcome(t *testing.T, kind string, out apicontract.Receipt, row stateport.Run) {
	t.Helper()
	if kind == "permanent" {
		if out.State != "failed" || row.DisplayReason != stateport.ReasonRetentionRefused {
			t.Fatal(out, row.DisplayReason)
		}
	} else if out.State != "accepted" || row.Status == "failed" || !strings.Contains(out.Warning, "stays accepted") || !strings.Contains(out.Warning, "next server start") {
		t.Fatal(out, row.Status)
	}
}
func retentionFailure(kind string) error {
	if kind == "permanent" {
		return permanentRetention("retention was refused")
	}
	cause := errors.New("retention storage is unavailable")
	if kind == "unavailable" {
		return publicerr.New(publicerr.Unavailable, "retention is unavailable", "Check source storage.", cause)
	}
	return cause
}
func permanentRetention(detail string) error {
	e := publicerr.New(publicerr.Forbidden, "source retention was refused", "Correct source storage access.", nil)
	e.Detail = detail
	return e
}
