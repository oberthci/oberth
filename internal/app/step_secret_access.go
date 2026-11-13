package app

import (
	"context"
	"errors"
	"fmt"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

// stepSecretAccess reloads current grants for the reviewed Workflow on every
// admission, including retries/reruns. Run metadata grants no secret authority.
func (jobs *ArgoJobs) stepSecretAccess(ctx context.Context, repository model.Repository, workflow *wfv1.Workflow, file string) (map[string]bool, map[string]map[string]bool, error) {
	approved := map[string]bool{}
	grants := map[string]map[string]bool{}
	paths, err := argoworkflow.DeclaredSecretPaths(workflow)
	if err != nil {
		return nil, nil, err
	}
	if !argojob.SecretStoreRequested(workflow) {
		return approved, grants, nil
	}
	if jobs.secretAccess == nil {
		return nil, nil, errors.New("app: secret-consuming steps require a secret access grant loader")
	}
	active, err := jobs.secretAccess.ActiveSecretGrants(ctx, repository.ID, file)
	if err != nil {
		return nil, nil, fmt.Errorf("app: load approved secrets for %s: %w", repository.Name, err)
	}
	for step, secrets := range active {
		for secret, allowed := range secrets {
			if !allowed {
				continue
			}
			approved[secret] = true
			if grants[secret] == nil {
				grants[secret] = map[string]bool{}
			}
			grants[secret][step] = true
		}
	}
	_, grantErr := argojob.GrantedSecretSteps(workflow, paths, grants)
	if grantErr != nil {
		return approved, grants, &argoworkflow.AdmissionError{Cause: grantErr}
	}
	return approved, grants, nil
}
