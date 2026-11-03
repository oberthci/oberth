package app

import (
	"context"
	"fmt"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	corev1 "k8s.io/api/core/v1"
)

// Every fresh submission snapshots its repository's administrator grant before
// audit and Build. No pipeline annotation participates. Lookup failure refuses
// submission; it never falls back to an earlier, more permissive grant.
func (jobs *ArgoJobs) bindResourceMaximum(ctx context.Context, submission *argojob.Request) error {
	if grants, ok := jobs.auditor.(interface {
		ResourceCeiling(context.Context, int64) (corev1.ResourceRequirements, error)
	}); ok {
		ceiling, err := grants.ResourceCeiling(ctx, submission.RepositoryID)
		if err != nil {
			return fmt.Errorf("read administrator resource ceiling: %w", err)
		}
		if err = argoworkflow.ValidateAdministratorResources(ceiling); err != nil {
			return err
		}
		submission.ResourceCeiling = ceiling.DeepCopy()
		submission.ResourceMaximum = ""
		return nil
	}
	maximum := store.DefaultMaxRunSize
	if grants, ok := jobs.auditor.(service.RunSizeLimiter); ok {
		var err error
		maximum, err = grants.MaxRunSize(ctx, submission.RepositoryID)
		if err != nil {
			return fmt.Errorf("read administrator resource ceiling: %w", err)
		}
	}
	if _, err := argoworkflow.ResourceCeilingForSize(maximum); err != nil {
		return err
	}
	submission.ResourceMaximum = maximum
	return nil
}
