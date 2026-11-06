// Package workflowpolicy guards removal of retired repository execution contracts.
package workflowpolicy

import (
	"context"
	"errors"
	"fmt"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

const ContractFile = ".oberth/tests.yaml"
const MaxContractBytes = 4 << 10

type PolicyGit interface {
	ReadOptionalBlob(context.Context, string, string, string, int) ([]byte, bool, error)
	ReadDefaultBlob(context.Context, string, string, int) (string, []byte, bool, error)
}

// Check refuses retired contracts in the candidate. A candidate deleting an
// upstream contract must remain runnable so it can be promoted normally.
func Check(ctx context.Context, git PolicyGit, repository, candidate string) (string, error) {
	if git == nil {
		return "", errors.New("workflow policy reader unavailable")
	}
	_, exists, err := git.ReadOptionalBlob(ctx, repository, candidate, ContractFile, MaxContractBytes)
	if err != nil && !errors.Is(err, gitcache.ErrPolicyBlobTooLarge) {
		return "", err
	}
	if exists {
		return "", argoworkflow.ErrWorkflowCapabilityRequired
	}
	baseline, _, _, err := git.ReadDefaultBlob(ctx, repository, ContractFile, MaxContractBytes)
	if errors.Is(err, gitcache.ErrPolicyUpstreamUnavailable) {
		return "", &PolicyRefreshUnavailable{Cause: err}
	}
	if err != nil && !errors.Is(err, gitcache.ErrPolicyBlobTooLarge) {
		return "", err
	}
	return baseline, nil
}

type PolicyRefreshUnavailable struct{ Cause error }

func (e *PolicyRefreshUnavailable) Error() string {
	return fmt.Sprintf("refresh workflow policy: %v", e.Cause)
}
func (e *PolicyRefreshUnavailable) Unwrap() error { return e.Cause }
