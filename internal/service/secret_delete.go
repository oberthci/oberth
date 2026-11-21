package service

import (
	"context"

	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/identityname"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
)

// A tierless grant may have stored this path under either account across route changes.
func (s *Service) deleteSecret(ctx context.Context, a actor.Actor, g stateport.GrantChange) error {
	if s.c.People.SecretWriter == nil {
		return secretDeleteRefusal("configuration", nil)
	}
	key := identityname.Key{Repository: g.Repository, File: g.File, Step: g.Step}
	q := apicontract.SecretPutArgs{Repo: g.Repository, Workflow: g.File, Step: g.Step, Secret: g.Path}
	var failure error
	for _, kind := range []identityname.Kind{identityname.StepCI, identityname.Step} {
		account, err := identityname.Name(key, kind)
		if err != nil {
			return secretDeleteRefusal("store", err)
		}
		tier := "release"
		if kind == identityname.StepCI {
			tier = "ci"
		}
		if err := s.deleteSecretAccount(ctx, a, secretAuditSelectors{SecretPutArgs: q, Tier: tier}, account); err != nil && failure == nil {
			failure = err
		}
	}
	return failure
}

func (s *Service) deleteSecretAccount(ctx context.Context, a actor.Actor, q secretAuditSelectors, account string) error {
	if err := s.secretAudit(ctx, a, "secret.delete.requested", q); err != nil {
		return secretDeleteRefusal("audit", nil)
	}
	if err := s.c.People.SecretWriter.Delete(ctx, account, q.Secret); err != nil {
		return secretDeleteRefusal("store", err)
	}
	return nil
}

func secretDeleteRefusal(code string, cause error) error {
	remedy := "Fix the store problem in detail, then repeat the revoke."
	detail := ""
	switch code {
	case "audit":
		remedy = "Restore audit access, then repeat the revoke to delete the value."
	case "configuration":
		remedy = "Configure the secret store writer, then repeat the revoke."
		detail = "Secret store writer is not configured."
	}
	if cause != nil {
		public := publicerr.Classify(cause)
		detail = public.Reason() + "\n" + public.Remedy()
	}
	err := publicerr.New(publicerr.Unavailable, "Grant revoked; secret still stored.", remedy, nil)
	err.Detail = detail
	return err
}
