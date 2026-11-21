package cronsync

import (
	"errors"
	"reflect"

	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/publicerr"
)

type diagnostic int

const (
	missingConfig diagnostic = iota
	foreignParent
	pendingSync
	badPrepared
	pinRefused
	childMismatch
	suspensionUnknown
	publicationFailed
	nativeFailure
	catalogFailure
	fileInvalid
	parentMissing
	branchMissing
	preparationFailed
	admissionUnavailable
	gitUnavailable
	pauseUnavailable
)

type message struct {
	code           publicerr.Code
	reason, remedy string
}

func messages() []message {
	return []message{
		{publicerr.Invariant, "schedule setup is incomplete", "Supply the schedule service dependencies."},
		{publicerr.Forbidden, "schedule belongs to another owner", "Choose a schedule managed by Oberth in this repository."},
		{publicerr.Unavailable, "schedule update is pending", "Retry after schedule permissions and preparation recover."},
		{publicerr.Invariant, "schedule details do not match", "Prepare this exact repository, revision and schedule entry."},
		{publicerr.InvalidInput, "scheduled source is not supported", "use a push or manual schedule trigger"},
		{publicerr.Forbidden, "scheduled run could not be verified", "Check the run's schedule and its current permissions before adopting it."},
		{publicerr.Unavailable, "some schedules may still run", "Retry the permissions change after all schedules can be paused."},
		{publicerr.Unavailable, "schedule permissions were not updated", "Retry the permissions change; schedules remain paused."},
		{publicerr.Unavailable, "schedule could not be updated", "Check the workflow controller and retry the schedule action."},
		{publicerr.Unavailable, "repositories could not be read", "Restore repository access and retry the schedule update."},
		{publicerr.InvalidInput, "schedule file is invalid", "Fix the listed values in .oberth/schedule.yaml and push to the default branch."},
		{publicerr.Unavailable, "schedule is not active", "Update the repository and check schedule details before retrying."},
		{publicerr.NotFound, "schedule branch not found", "Push the branch to the oberth remote: git push oberth <branch>."},
		{publicerr.InvalidInput, "schedule cannot be prepared", "Fix the named file or input and push to the default branch."},
		{publicerr.Unavailable, "schedule admission is unavailable", "Restore Git, policy and store access, then retry the schedule update."},
		{publicerr.Unavailable, "schedule Git is unavailable", "Restore repository access and retry the schedule update."},
		{publicerr.Unavailable, "schedule pause intent is unavailable", "Restore audit access and retry the schedule action."},
	}
}
func problem(id diagnostic, cause error) *publicerr.Error {
	m := messages()[id]
	return publicerr.New(m.code, m.reason, m.remedy, cause)
}

// ChildRefusal marks a permanent proof mismatch, distinct from native unavailability.
type ChildRefusal struct{ cause error }

func (e *ChildRefusal) Error() string { return problem(childMismatch, nil).Error() }
func (e *ChildRefusal) Unwrap() error { return problem(childMismatch, e.cause) }

// unstarted marks a full pass that failed before it reached any repository:
// the catalogue or the native LIST could not be read.
type unstarted struct{ error }

func (e unstarted) Unwrap() error { return e.error }

// Unstarted reports whether err is a full pass that reached no repository,
// which no later event repeats; per-repository failures are not.
func Unstarted(err error) bool {
	var failure unstarted
	return errors.As(err, &failure)
}

func transientError(err error) error {
	if publicerr.Permanent(err) {
		return nil
	}
	return err
}
func optionalFile(err error, path string) bool {
	if err == nil {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, child := range joined.Unwrap() {
			if !optionalFile(child, path) {
				return false
			}
		}
		return len(joined.Unwrap()) > 0
	}
	var absent *gitport.AbsentError
	if reflect.TypeOf(err) == reflect.TypeFor[*gitport.AbsentError]() && errors.As(err, &absent) {
		return absent != nil && absent.Path == path
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok && wrapped.Unwrap() != nil {
		return optionalFile(wrapped.Unwrap(), path)
	}
	return false
}

func displayError(failure *publicerr.Error) string {
	text := failure.Reason() + " - " + failure.Remedy()
	if failure.Detail != "" {
		text += "\n" + failure.Detail
	}
	return text
}

// refusal retains the producing stage alongside the classified public error.
type refusal struct {
	err    *publicerr.Error
	status diagnostic
}

func admissionFailure(cause error) (diagnostic, *publicerr.Error) {
	failure := publicerr.Classify(cause)
	if publicerr.Permanent(cause) {
		return preparationFailed, failure
	}
	if failure.Code() == publicerr.Invariant {
		failure = problem(admissionUnavailable, cause)
	}
	return admissionUnavailable, failure
}
func (c *Syncer) remember(id int64, entry string, status diagnostic, cause *publicerr.Error) {
	if c.refusals[id] == nil {
		c.refusals[id] = map[string]*refusal{}
	}
	c.refusals[id][entry] = &refusal{cause, status}
}
func (c *Syncer) failure(id int64, entry string) *refusal {
	if failure := c.refusals[id][entry]; failure != nil {
		return failure
	}
	return c.refusals[id][""]
}
