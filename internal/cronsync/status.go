package cronsync

import (
	"context"
	"maps"
	"slices"
	"strings"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/schedule"
)

// Status keeps generated Error phrases short; Detail and Conditions retain native diagnostics.
type Status struct {
	Entry             string
	Cron              string
	Branch            string
	Timezone          string
	Name              string
	UserPaused        bool
	SyncSuspended     bool
	Error             string
	Remedy            string
	Detail            string
	Conditions        wfv1.Conditions
	LastScheduledTime *metav1.Time
	Next              time.Time
}

// Status reads native facts without repair or firing authority.
func (c *Syncer) Status(ctx context.Context, repo Repository, entry string) (Status, error) {
	if err := c.acquire(ctx); err != nil {
		return Status{}, err
	}
	defer c.release()
	return c.status(ctx, repo, entry)
}
func (c *Syncer) status(ctx context.Context, repo Repository, entry string) (Status, error) {
	name, err := nativebinding.CronName(repo.ID, entry)
	if err != nil {
		return Status{}, err
	}
	result := Status{Name: name, Entry: entry}
	failure := c.failure(repo.ID, entry)
	if failure != nil {
		refusalStatus(&result, failure)
		result.SyncSuspended = publicerr.Permanent(failure.err) || failure.status == pendingSync
	}
	object, err := c.config.Client.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		if result.Error == "" {
			statusError(&result, parentMissing, messages()[parentMissing].remedy)
		}
		return result, nil
	}
	if err != nil {
		return result, problem(nativeFailure, err)
	}
	if _, _, ok := owned(object); !ok || object.Namespace != c.config.Namespace {
		return result, problem(foreignParent, nil)
	}
	if err := c.projectNative(ctx, repo, &result, object); err != nil {
		return result, err
	}
	if len(object.Spec.Schedules) == 1 {
		c.nextTime(&result, object)
	}
	return result, nil
}
func (c *Syncer) nextTime(result *Status, object *wfv1.CronWorkflow) {
	if object.Spec.Suspend {
		return
	}
	next, err := (schedule.Entry{Cron: object.Spec.Schedules[0], Timezone: object.Spec.Timezone}).Next(c.config.Now())
	if err != nil {
		statusError(result, nativeFailure, err.Error())
		return
	}
	result.Next = next.UTC()
}

// Snapshot retains sorted native/refusal facts even when declaration reads fail.
func (c *Syncer) Snapshot(ctx context.Context, repo Repository) ([]Status, error) {
	if err := c.acquire(ctx); err != nil {
		return nil, err
	}
	defer c.release()
	objects, err := c.list(ctx)
	if err != nil {
		return nil, err
	}
	entries := map[string]bool{}
	for i := range objects {
		id, entry, ok := owned(&objects[i])
		if ok && id == repo.ID {
			entries[entry] = true
		}
	}
	for entry := range c.refusals[repo.ID] {
		if entry != "" {
			entries[entry] = true
		}
	}
	result := []Status{}
	if root := c.refusals[repo.ID][""]; root != nil {
		var status Status
		refusalStatus(&status, root)
		result = append(result, status)
	}
	for _, entry := range slices.Sorted(maps.Keys(entries)) {
		status, err := c.status(ctx, repo, entry)
		if err != nil {
			return nil, err
		}
		result = append(result, status)
	}
	return result, nil
}

func (c *Syncer) projectNative(ctx context.Context, repo Repository, result *Status, object *wfv1.CronWorkflow) error {
	paused, err := c.config.PauseIntent(ctx, repo.Qualified, result.Entry)
	if err != nil {
		return problem(pauseUnavailable, err)
	}
	result.SyncSuspended = object.Spec.Suspend && !paused || result.SyncSuspended
	result.Cron = strings.Join(object.Spec.Schedules, ", ")
	result.Timezone = object.Spec.Timezone
	result.UserPaused = paused
	if object.Spec.WorkflowMetadata != nil {
		result.Branch = object.Spec.WorkflowMetadata.Annotations[nativebinding.RefAnnotation]
	}
	result.LastScheduledTime = object.Status.LastScheduledTime.DeepCopy()
	result.Conditions = slices.Clone(object.Status.Conditions)
	for _, condition := range result.Conditions {
		if condition.Status == metav1.ConditionTrue && (condition.Type == wfv1.ConditionTypeSpecError || condition.Type == "SubmissionError") {
			statusError(result, nativeFailure, string(condition.Type)+": "+condition.Message)
		}
	}
	return nil
}

func statusError(status *Status, id diagnostic, detail string) {
	status.Error = messages()[id].reason
	status.Remedy = messages()[id].remedy
	if status.Detail != "" {
		status.Detail += "\n"
	}
	status.Detail += detail
}

func refusalStatus(status *Status, refusal *refusal) {
	statusError(status, refusal.status, displayError(refusal.err))
	status.Remedy = refusal.err.Remedy()
}

// BranchMissing reports an entry refused because its branch was not found:
// the one state a fresh look at the forge can change.
func BranchMissing(status Status) bool { return status.Error == messages()[branchMissing].reason }

// NotAdmitted refuses an occurrence of an entry that reconciliation has not
// admitted, in the entry's own reason and remedy.
func NotAdmitted(status Status) *publicerr.Error {
	reason, remedy := status.Error, status.Remedy
	if reason == "" {
		reason, remedy = messages()[pendingSync].reason, messages()[pendingSync].remedy
	}
	failure := publicerr.New(publicerr.Conflict, "Schedule not admitted: "+reason+".", remedy, nil)
	failure.Detail = status.Detail
	return failure
}
