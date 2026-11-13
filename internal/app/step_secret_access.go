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
	// Snapshot health before reading grants. Initial reconciliation may revoke
	// stale grants; a read from before that reconciliation must not become
	// authoritative merely because health turns true while the read completes.
	jobs.mu.Lock()
	healthy := jobs.reconcilerHealthy == nil || jobs.reconcilerHealthy.ReconcileHealthy()
	jobs.mu.Unlock()
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
	steps, grantErr := argojob.GrantedSecretSteps(workflow, paths, grants)
	blocked := len(steps) > 0 && !healthy
	if blocked {
		return nil, nil, fmt.Errorf("app: access reconciler has not completed initial reconciliation; secret access is blocked for %s", repository.Name)
	}
	if grantErr != nil {
		return approved, grants, &argoworkflow.AdmissionError{Cause: grantErr}
	}
	return approved, grants, nil
}
