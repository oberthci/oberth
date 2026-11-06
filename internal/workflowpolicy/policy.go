// Package workflowpolicy guards removal of retired repository execution contracts.
package workflowpolicy

import (
	"context"
	"errors"
	"fmt"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

const ContractFile = ".oberth/tests.yaml"
const MaxContractBytes = 4 << 10

type PolicyGit interface {
	ReadOptionalBlob(context.Context, string, string, string, int) ([]byte, bool, error)
	ReadDefaultBlob(context.Context, string, string, int) (string, []byte, bool, error)
}

// Check refuses both candidate and current upstream legacy requirements. Their
// disappearance from a candidate cannot silently bypass a published gate.
func Check(ctx context.Context, git PolicyGit, repository, candidate string) (string, error) {
	if git == nil {
		return "", errors.New("workflow policy reader unavailable")
	}
	_, exists, err := git.ReadOptionalBlob(ctx, repository, candidate, ContractFile, MaxContractBytes)
	if err != nil {
		return "", err
	}
	if exists {
		return "", argoworkflow.ErrWorkflowCapabilityRequired
	}
	baseline, _, exists, err := git.ReadDefaultBlob(ctx, repository, ContractFile, MaxContractBytes)
	if err != nil {
		return "", &PolicyRefreshUnavailable{Cause: err}
	}
	if exists {
		return "", argoworkflow.ErrWorkflowCapabilityRequired
	}
	return baseline, nil
}

type PolicyRefreshUnavailable struct{ Cause error }

func (e *PolicyRefreshUnavailable) Error() string {
	return fmt.Sprintf("refresh workflow policy: %v", e.Cause)
}
func (e *PolicyRefreshUnavailable) Unwrap() error { return e.Cause }
