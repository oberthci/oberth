package app_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/nativebinding"
)

// With assist forced off, a push carrying the assist option must leave a run
// that is ordinary evidence and a Workflow with no helper.
func TestReviewIgnoredAssistLeavesOrdinaryRun(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	_ = expectAssistOff(t, f)
	var sealed []byte
	create := f.native.CreateFunc
	f.native.CreateFunc = func(ctx context.Context, p *admissioncontract.PreparedAdmission) (nativebinding.RunBinding, error) {
		sealed, _ = p.MarshalJSON()
		return create(ctx, p)
	}
	s := f.new(t)
	r := receive()
	r.Source = f.source
	r.Updates[0].Request.Assist = true
	if _, err := s.Gate(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Deliver(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("oberth-hold")) || bytes.Contains(sealed, []byte("OBERTH_HOLD_")) {
		t.Fatal("helper present in sealed workflow")
	}
	run, err := f.runs.Run(t.Context(), "r1")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("origin: requested=%v assisted=%v hasAssistance=%v", run.Origin.AssistRequested, run.Origin.Assisted, run.Origin.HasAssistance())
	if run.Origin.HasAssistance() {
		t.Fatal("ignored assist option still excludes the run from green/promotion evidence")
	}
}
