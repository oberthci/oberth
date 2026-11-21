package service

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/identityname"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
)

func (s *Service) PrepareSecretPut(ctx context.Context, c apicontract.Caller, q apicontract.SecretPutArgs) (apicontract.Call[apicontract.Receipt], error) {
	a, err := s.peopleAllow(ctx, c.Actor, "secret_put")
	if err != nil {
		return nil, err
	}
	repo, err := s.resolveRepo(ctx, a, q.Repo)
	if err != nil {
		return nil, err
	}
	q.Repo = repo.Canonical
	if _, err := grantInput(apicontract.AccessAllowArgs{Repo: q.Repo, Workflow: q.Workflow, Step: q.Step, Secret: q.Secret}); err != nil {
		return nil, err
	}
	value, ok := apicontract.SecretValue(ctx)
	if !ok {
		return nil, secretRefusal("body")
	}
	if len(value) > model.MaxSecretValueBytes {
		return nil, secretRefusal("size")
	}
	if s.c.People.SecretWriter == nil {
		return nil, refuse(unavailable)
	}
	return bounded(func(ctx context.Context) (apicontract.Receipt, error) {
		// Resolve authority at execution time so a revoked grant cannot authorize a prepared call.
		live, err := s.peopleAllow(ctx, a, "secret_put")
		if err != nil {
			return apicontract.Receipt{}, err
		}
		return s.writeSecret(ctx, live, q, value)
	}), nil
}

func (s *Service) writeSecret(ctx context.Context, a actor.Actor, q apicontract.SecretPutArgs, value []byte) (apicontract.Receipt, error) {
	release, err := s.acquire(ctx, secretKey(q.Repo))
	if err != nil {
		return apicontract.Receipt{}, err
	}
	defer release()
	snapshot, err := s.c.People.Grants.Grants(ctx, []string{q.Repo})
	if err != nil {
		return apicontract.Receipt{}, err
	}
	granted := slices.ContainsFunc(snapshot.Grants, func(g stateport.GrantChange) bool {
		return secretGranted(g, q)
	})
	if !granted {
		return apicontract.Receipt{}, secretRefusal("grant")
	}
	account, err := s.secretAccount(identityname.Key{Repository: q.Repo, File: q.Workflow, Step: q.Step})
	if err != nil {
		return apicontract.Receipt{}, bad("grant")
	}
	if err := s.secretAudit(ctx, a, "secret.put.requested", secretAuditSelectors{SecretPutArgs: q}); err != nil {
		return apicontract.Receipt{}, err
	}
	if err := s.c.People.SecretWriter.Put(ctx, account, q.Secret, value); err != nil {
		var public *publicerr.Error
		if errors.As(err, &public) {
			return apicontract.Receipt{}, public
		}
		return apicontract.Receipt{}, secretRefusal("write")
	}
	return apicontract.Receipt{ID: q.Secret, State: "stored"}, nil
}

func secretKey(repo string) string {
	return "secret:" + repo
}

func (s *Service) secretAccount(key identityname.Key) (string, error) {
	kind := identityname.StepCI
	if slices.Contains(s.c.People.ReleaseRoutes.Files(key.Repository), key.File) {
		kind = identityname.Step
	}
	return identityname.Name(key, kind)
}

type secretAuditSelectors struct {
	apicontract.SecretPutArgs
	Tier string `json:"tier,omitempty"`
}

// Secret audits record only public selectors before the external request.
func (s *Service) secretAudit(ctx context.Context, a actor.Actor, action string, q secretAuditSelectors) error {
	details, _ := json.Marshal(q)
	fact := model.AuditAction{Actor: a.Identity, Action: action, ResourceType: "repository", ResourceID: q.Repo, Details: string(details)}
	return s.c.People.Admin.RecordAdministration(ctx, s.peopleAction(a, "administration", "", q.Repo), fact)
}

func secretGranted(g stateport.GrantChange, q apicontract.SecretPutArgs) bool {
	return g.Kind == stateport.GrantStorePath && !g.Revoke && !g.Dismiss && g.Repository == q.Repo && g.File == q.Workflow && g.Step == q.Step && g.Path == q.Secret
}

func secretRefusal(code string) error {
	reason, remedy := "Secret store refused the write.", "Run oberth secretstore check, then retry."
	kind := publicerr.Unavailable
	switch code {
	case "body":
		reason, remedy, kind = "A private secret body is required.", "Send the value in the POST /api/secrets/put body.", publicerr.InvalidInput
	case "size":
		reason, remedy, kind = "The secret exceeds 1 MiB.", "Supply a value of at most 1 MiB.", publicerr.InvalidInput
	case "grant":
		reason, remedy, kind = "The secret path is not granted.", "Approve this repository, file, step and path with oberth access allow.", publicerr.Forbidden
	}
	return publicerr.New(kind, reason, remedy, nil)
}
