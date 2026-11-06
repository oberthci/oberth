package workflowpolicy

import (
	"context"
	"errors"
	"testing"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

type policyFixture struct {
	candidate, baseline bool
	failure             error
}

func (g policyFixture) ReadOptionalBlob(context.Context, string, string, string, int) ([]byte, bool, error) {
	return nil, g.candidate, nil
}
func (g policyFixture) ReadDefaultBlob(context.Context, string, string, int) (string, []byte, bool, error) {
	return "baseline", nil, g.baseline, g.failure
}
func TestRetiredPolicyCannotBeDeletedByCandidate(t *testing.T) {
	for _, g := range []policyFixture{{candidate: true}, {baseline: true}} {
		if _, err := Check(t.Context(), g, "repo", "candidate"); !errors.Is(err, argoworkflow.ErrWorkflowCapabilityRequired) {
			t.Fatal(err)
		}
	}
	if baseline, err := Check(t.Context(), policyFixture{}, "repo", "candidate"); err != nil || baseline != "baseline" {
		t.Fatal(baseline, err)
	}
	var unavailable *PolicyRefreshUnavailable
	if _, err := Check(t.Context(), policyFixture{failure: errors.New("offline")}, "repo", "candidate"); !errors.As(err, &unavailable) {
		t.Fatal(err)
	}
}
