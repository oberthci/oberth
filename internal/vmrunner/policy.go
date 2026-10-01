package vmrunner

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/gitoid"
)

// PolicyGit reads immutable candidate objects and freshly fetched, private
// upstream policy. A checkout or publicly pushable cache ref is not policy.
type PolicyGit interface {
	ReadOptionalBlob(context.Context, string, string, string, int) ([]byte, bool, error)
	ReadDefaultBlob(context.Context, string, string, int) (string, []byte, bool, error)
}

type Requirement struct {
	CandidateSHA string
	BaselineSHA  string
	Contract     *Contract
}

// ResolveRequirement retains published requirements when the candidate removes
// the declaration. Read failures are errors, never evidence of absence.
func ResolveRequirement(ctx context.Context, git PolicyGit, repository, candidateSHA string) (Requirement, error) {
	if git == nil || strings.TrimSpace(repository) == "" || !gitoid.Valid(candidateSHA) {
		return Requirement{}, errors.New("trusted test policy requires Git, repository and exact candidate SHA")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	candidateBody, candidateExists, err := git.ReadOptionalBlob(ctx, repository, candidateSHA, ContractFile, MaxContractBytes)
	if err != nil {
		return Requirement{}, fmt.Errorf("read candidate trusted test policy: %w", err)
	}
	candidate, err := decodeOptionalContract(candidateBody, candidateExists)
	if err != nil {
		return Requirement{}, fmt.Errorf("candidate trusted test policy: %w", err)
	}
	baselineSHA, baselineBody, baselineExists, err := git.ReadDefaultBlob(ctx, repository, ContractFile, MaxContractBytes)
	if err != nil {
		return Requirement{}, fmt.Errorf("refresh upstream trusted test policy: %w", err)
	}
	if (baselineSHA != "" && !gitoid.Valid(baselineSHA)) || (baselineExists && baselineSHA == "") {
		return Requirement{}, errors.New("upstream trusted test policy has no exact commit identity")
	}
	baseline, err := decodeOptionalContract(baselineBody, baselineExists)
	if err != nil {
		return Requirement{}, fmt.Errorf("upstream trusted test policy: %w", err)
	}
	contract, err := RequiredContract(candidate, baseline)
	if err != nil {
		return Requirement{}, err
	}
	return Requirement{CandidateSHA: candidateSHA, BaselineSHA: baselineSHA, Contract: contract}, nil
}

func decodeOptionalContract(body []byte, exists bool) (*Contract, error) {
	if !exists {
		if len(body) != 0 {
			return nil, errors.New("absent trusted test policy returned content")
		}
		return nil, nil
	}
	contract, err := DecodeContract(body)
	if err != nil {
		return nil, err
	}
	return &contract, nil
}
