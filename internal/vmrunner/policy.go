package vmrunner

import (
	"context"

	"github.com/oberthci/oberth/internal/protectedtests"
)

type PolicyGit = protectedtests.PolicyGit
type Requirement = protectedtests.Requirement
type PolicyRefreshUnavailable = protectedtests.PolicyRefreshUnavailable

func ResolveRequirement(ctx context.Context, git PolicyGit, repository, candidateSHA string, authorizeRotation func(Contract) error) (Requirement, error) {
	return protectedtests.ResolveRequirement(ctx, git, repository, candidateSHA, authorizeRotation)
}
