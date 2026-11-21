package service

import (
	"context"
	"errors"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
)

func (s *Service) removeRepository(ctx context.Context, caller apicontract.Caller, repo stateport.Repository, action stateport.Action) error {
	release, err := s.acquire(ctx, secretKey(repo.Canonical))
	if err != nil {
		return err
	}
	defer release()
	if err := s.c.People.Admin.CheckRepositoryRemoval(ctx, repo.ID); err != nil {
		return err
	}
	snapshot, err := s.c.People.Grants.Grants(ctx, []string{repo.Canonical})
	if err != nil {
		return err
	}
	revoked := false
	for _, grant := range snapshot.Grants {
		if grant.Kind != stateport.GrantStorePath || grant.Dismiss {
			continue
		}
		// Revoked rows can retain values after a failed delete; removal retries them.
		grant.Revoke = true
		incomplete, err := s.changeGrant(ctx, caller, grant, action.Reason)
		if err != nil {
			return err
		}
		if incomplete {
			return refuse(repositoryRevocationPending)
		}
		revoked = true
	}
	err = s.c.People.Admin.RemoveRepository(ctx, action, repo.ID)
	var failure *publicerr.Error
	if revoked && errors.As(err, &failure) {
		problem := publicerr.New(failure.Code(), failure.Reason(),
			"Secret grants were already revoked; resolve the repository removal refusal and retry.", failure.Unwrap())
		problem.Detail = failure.Remedy()
		return problem
	}
	return err
}
