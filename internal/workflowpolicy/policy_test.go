package workflowpolicy

import (
	"context"
	"errors"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

type policyFixture struct {
	candidate, baseline   bool
	candidateErr, failure error
}

func (g policyFixture) ReadOptionalBlob(context.Context, string, string, string, int) ([]byte, bool, error) {
	return nil, g.candidate, g.candidateErr
}
func (g policyFixture) ReadDefaultBlob(context.Context, string, string, int) (string, []byte, bool, error) {
	return "baseline", nil, g.baseline, g.failure
}
func TestCandidateMayDeleteRetiredDefaultContract(t *testing.T) {
	for _, g := range []policyFixture{{}, {baseline: true}, {baseline: true, failure: gitcache.ErrPolicyBlobTooLarge}} {
		if baseline, err := Check(t.Context(), g, "repo", "candidate"); err != nil || baseline != "baseline" {
			t.Fatal(baseline, err)
		}
	}
	for _, g := range []policyFixture{{candidate: true}, {candidate: true, candidateErr: gitcache.ErrPolicyBlobTooLarge}} {
		if _, err := Check(t.Context(), g, "repo", "candidate"); !errors.Is(err, argoworkflow.ErrWorkflowCapabilityRequired) {
			t.Fatal(err)
		}
	}
}
func TestOnlyUpstreamOutagesWait(t *testing.T) {
	for _, cause := range []error{gitcache.ErrPolicyUpstreamUnavailable, context.Canceled, errors.New("corrupt object"), gitcache.ErrPolicyRepositoryNotCached} {
		_, err := Check(t.Context(), policyFixture{failure: cause}, "repo", "candidate")
		var unavailable *PolicyRefreshUnavailable
		if !errors.Is(err, cause) || errors.As(err, &unavailable) != errors.Is(cause, gitcache.ErrPolicyUpstreamUnavailable) {
			t.Fatalf("cause=%v error=%v", cause, err)
		}
	}
}
