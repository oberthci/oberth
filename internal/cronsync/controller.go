// Package cronsync reconciles admitted schedules and verifies native child ownership.
// The lock is not a run cap; the native controller owns firing and execution.
// Spec: docs/plans/c10-schedules.md#owned-data-and-public-contract
package cronsync

import (
	"context"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/schedule"
)

type Repository struct {
	ID                       int64
	Qualified, DefaultBranch string
	Archived                 bool
}

// Git is the bounded, exact-revision subset implemented by the shared Git adapter.
// ReadFile returns at most 1 MiB; only AbsentError with the requested path means a missing file.
type Git interface {
	ResolveRef(context.Context, string, string) (gitport.Ref, error)
	ReadFile(context.Context, gitport.Revision, string) (gitport.File, error)
}

// Native is namespace-scoped; reads/writes detach values and honor UID/resourceVersion.
type Native interface {
	Get(context.Context, string, metav1.GetOptions) (*wfv1.CronWorkflow, error)
	List(context.Context, metav1.ListOptions) (*wfv1.CronWorkflowList, error)
	Create(context.Context, *wfv1.CronWorkflow, metav1.CreateOptions) (*wfv1.CronWorkflow, error)
	Update(context.Context, *wfv1.CronWorkflow, metav1.UpdateOptions) (*wfv1.CronWorkflow, error)
	Delete(context.Context, string, metav1.DeleteOptions) error
}

type Preparation struct {
	Repository Repository
	Entry      schedule.Entry
	Revision   gitport.Ref
}

// Config requires production readers and C3 preparation/C4 native validation.
// PauseIntent reads the latest committed audit intent; true means explicitly paused.
// PublishAdmission prunes snapshots only after the native apply succeeds.
type Config struct {
	Namespace            string
	InstanceID           string
	Client               Native
	Git                  Git
	Repositories         func(context.Context) ([]Repository, error)
	Repository           func(context.Context, string) (Repository, bool, error)
	Admit                func(context.Context, Preparation) (*admissioncontract.PreparedAdmission, error)
	RetainAdmission      func(context.Context, *admissioncontract.PreparedAdmission) error
	PublishAdmission     func(context.Context, int64, string, []string) error
	ForgetAdmission      func(context.Context, int64, string) error
	PruneAdmissions      func(context.Context, int64, []string) error
	ValidateNativeSchema func(context.Context, *wfv1.CronWorkflow, []admissioncontract.TemplateSnapshot) error
	PauseIntent          func(context.Context, string, string) (bool, error)
	Limits               schedule.Limits
	Now                  func() time.Time
}

// Syncer serializes control-plane operations. Callbacks must not reenter it.
type Syncer struct {
	config         Config
	owner          chan struct{}
	refusals       map[int64]map[string]*refusal
	fullSyncFailed bool
}

func New(config Config) (*Syncer, error) {
	if !config.complete() {
		return nil, problem(missingConfig, nil)
	}
	if err := config.Limits.Validate(); err != nil {
		return nil, err
	}
	return &Syncer{config: config, owner: make(chan struct{}, 1), refusals: map[int64]map[string]*refusal{}}, nil
}

func (c Config) complete() bool {
	return c.Namespace != "" && c.Client != nil && c.Git != nil && c.Repositories != nil && c.Repository != nil && c.Admit != nil && c.ValidateNativeSchema != nil && c.PauseIntent != nil && c.Now != nil && c.admissionsConfigured()
}

func (c Config) admissionsConfigured() bool {
	return c.RetainAdmission != nil && c.PublishAdmission != nil && c.ForgetAdmission != nil && c.PruneAdmissions != nil
}

func (c *Syncer) acquire(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case c.owner <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (c *Syncer) release() { <-c.owner }
