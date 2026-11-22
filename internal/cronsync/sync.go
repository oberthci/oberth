package cronsync

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/schedule"
)

func (c *Syncer) list(ctx context.Context) ([]wfv1.CronWorkflow, error) {
	objects, err := c.config.Client.List(ctx, metav1.ListOptions{LabelSelector: nativebinding.ManagedLabel + "=" + nativebinding.ManagedValue})
	if err != nil {
		return nil, problem(nativeFailure, err)
	}
	return objects.Items, nil
}

// CachedGit is a Git that can also resolve a ref from what it already holds.
// The relevance check of RefsChanged uses it: a forge refresh there would take a
// move away from the notice that has to report it.
type CachedGit interface {
	CachedRef(context.Context, string, string) (gitport.Ref, error)
}

func (c *Syncer) declaration(ctx context.Context, repo Repository) (schedule.File, *publicerr.Error) {
	return c.declarationAt(ctx, repo, c.config.Git.ResolveRef)
}

// cachedDeclaration reads the declaration without refreshing when Git can.
func (c *Syncer) cachedDeclaration(ctx context.Context, repo Repository) (schedule.File, *publicerr.Error) {
	if cached, ok := c.config.Git.(CachedGit); ok {
		return c.declarationAt(ctx, repo, cached.CachedRef)
	}
	return c.declaration(ctx, repo)
}

func (c *Syncer) declarationAt(ctx context.Context, repo Repository, resolve func(context.Context, string, string) (gitport.Ref, error)) (schedule.File, *publicerr.Error) {
	ref, err := resolve(ctx, repo.Qualified, "refs/heads/"+repo.DefaultBranch)
	if err != nil {
		_, failure := branchFailure("", repo.DefaultBranch, err)
		return schedule.File{}, failure
	}
	revision := gitport.Revision{Repository: repo.Qualified, Commit: ref.CommitSHA}
	file, err := c.config.Git.ReadFile(ctx, revision, schedule.FileName)
	if optionalFile(err, schedule.FileName) {
		return schedule.File{}, nil
	}
	if err != nil {
		if publicerr.Permanent(err) {
			failure := problem(fileInvalid, err)
			failure.Detail = fmt.Sprintf("file: field %q has value %q: restore this file at the default-branch head or remove its declaration.", "path", schedule.FileName)
			return schedule.File{}, failure
		}
		return schedule.File{}, problem(gitUnavailable, err)
	}
	result, err := schedule.Parse(file.Bytes, c.config.Limits, c.config.Now())
	return result, publicerr.Classify(err)
}
// syncAll reconciles every repository, each within its own RepositoryAttempt,
// so a slow one uses up only its own time; a repository the pass no longer
// reaches within ctx keeps its state rather than being recorded as failed.
// Resumes are let through only by a pass that went over every repository: a
// pass that runs out of time keeps the block it found (after a restart, the
// one set at construction).
func (c *Syncer) syncAll(ctx context.Context) error {
	prior := c.fullSyncFailed
	repos, objects, err := c.passStart(ctx)
	if err != nil {
		return err
	}
	reached := true
	var failures []error
	for _, repo := range repos {
		if err := ctx.Err(); err != nil {
			failures = append(failures, err)
			reached = false
			continue
		}
		failures = append(failures, c.attempt(ctx, repo, objects))
	}
	failures = append(failures, c.removeOrphans(ctx, repos, objects))
	c.fullSyncFailed = prior && !reached
	return errors.Join(failures...)
}

// passStart reads the complete catalogue and native list a full pass works
// from; until the pass has gone over every repository, resumes are refused.
func (c *Syncer) passStart(ctx context.Context) ([]Repository, []wfv1.CronWorkflow, error) {
	c.fullSyncFailed = true
	repos, err := c.config.Repositories(ctx)
	if err != nil {
		return nil, nil, unreadable{problem(catalogFailure, err)}
	}
	objects, err := c.list(ctx)
	if err != nil {
		return nil, nil, unreadable{err}
	}
	return repos, objects, nil
}

// removeOrphans deletes the owned parents of repositories absent from the
// complete catalogue repos.
func (c *Syncer) removeOrphans(ctx context.Context, repos []Repository, objects []wfv1.CronWorkflow) error {
	present := map[int64]bool{}
	for _, repo := range repos {
		present[repo.ID] = true
	}
	var failures []error
	for i := range objects {
		object := &objects[i]
		id, _, ok := owned(object)
		if ok && !present[id] {
			failures = append(failures, c.remove(ctx, object))
			delete(c.refusals, id)
		}
	}
	return errors.Join(failures...)
}

func (c *Syncer) attempt(ctx context.Context, repo Repository, objects []wfv1.CronWorkflow) error {
	if c.config.RepositoryAttempt > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.config.RepositoryAttempt)
		defer cancel()
	}
	return c.syncRepository(ctx, repo, objects)
}

// syncOne reconciles one repository. A key the catalogue no longer knows
// runs the full pass, which carries a renamed repository.
func (c *Syncer) syncOne(ctx context.Context, key string) error {
	repo, found, err := c.config.Repository(ctx, key)
	if err != nil {
		return problem(catalogFailure, err)
	}
	if !found {
		return c.syncAll(ctx)
	}
	objects, err := c.list(ctx)
	if err != nil {
		return err
	}
	return c.syncRepository(ctx, repo, objects)
}

func (c *Syncer) syncRepository(ctx context.Context, repo Repository, objects []wfv1.CronWorkflow) error {
	existing := map[string]*wfv1.CronWorkflow{}
	for i := range objects {
		id, entry, ok := owned(&objects[i])
		if ok && id == repo.ID {
			existing[entry] = &objects[i]
		}
	}
	if c.refusals[repo.ID] == nil {
		c.refusals[repo.ID] = map[string]*refusal{}
	}
	file := schedule.File{}
	var err *publicerr.Error
	if !repo.Archived {
		file, err = c.declaration(ctx, repo)
	}
	if err != nil {
		return c.refuseRepository(ctx, repo, existing, err)
	}
	delete(c.refusals[repo.ID], "")
	return c.syncEntries(ctx, repo, file, existing)
}
func (c *Syncer) syncEntries(ctx context.Context, repo Repository, file schedule.File, existing map[string]*wfv1.CronWorkflow) error {
	wanted := map[string]bool{}
	var failures []error
	for _, refusal := range file.Refused {
		wanted[refusal.Name] = true
		failures = append(failures, c.refuseEntry(ctx, repo.ID, refusal.Name, entryFailure{existing[refusal.Name], publicerr.Classify(refusal.Error), fileInvalid}))
	}
	for _, entry := range file.Entries {
		wanted[entry.Name] = true
		delete(c.refusals[repo.ID], entry.Name)
		if reason, err := c.syncEntry(ctx, repo, entry); err != nil {
			failures = append(failures, c.refuseEntry(ctx, repo.ID, entry.Name, entryFailure{existing[entry.Name], err, reason}))
		}
	}
	pruneErr := c.prune(ctx, repo.ID, wanted, existing)
	failures = append(failures, pruneErr)
	if pruneErr == nil {
		failures = append(failures, c.config.PruneAdmissions(ctx, repo.ID, slices.Sorted(maps.Keys(wanted))))
	}
	return errors.Join(failures...)
}
func (c *Syncer) prune(ctx context.Context, id int64, wanted map[string]bool, existing map[string]*wfv1.CronWorkflow) error {
	for entry := range c.refusals[id] {
		if !wanted[entry] {
			delete(c.refusals[id], entry)
		}
	}
	var failures []error
	for entry, object := range existing {
		if !wanted[entry] {
			failures = append(failures, c.remove(ctx, object))
		}
	}
	return errors.Join(failures...)
}
func (c *Syncer) syncEntry(ctx context.Context, repo Repository, entry schedule.Entry) (diagnostic, *publicerr.Error) {
	branch := entry.Branch
	if branch == "" {
		branch = repo.DefaultBranch
	}
	revision, err := c.config.Git.ResolveRef(ctx, repo.Qualified, "refs/heads/"+branch)
	if err != nil {
		return branchFailure(entry.Name, branch, err)
	}
	request := Preparation{repo, entry, revision}
	prepared, err := c.config.Admit(ctx, request)
	if err != nil {
		return admissionFailure(err)
	}
	object, err := render(c.config.Namespace, c.config.InstanceID, request, prepared)
	if err != nil {
		return fileInvalid, publicerr.Classify(err)
	}
	if err := c.config.ValidateNativeSchema(ctx, object.DeepCopy(), prepared.View().Templates); err != nil {
		return admissionFailure(err)
	}
	if err := c.config.RetainAdmission(ctx, prepared); err != nil {
		return admissionUnavailable, problem(admissionUnavailable, err)
	}
	previous, err := c.apply(ctx, request, object)
	if err != nil {
		return nativeFailure, problem(nativeFailure, err)
	}
	if err := c.config.PublishAdmission(ctx, repo.ID, entry.Name, []string{prepared.View().Digest, previous}); err != nil {
		return admissionUnavailable, problem(admissionUnavailable, err)
	}
	return 0, nil
}

type entryFailure struct {
	object *wfv1.CronWorkflow
	cause  *publicerr.Error
	status diagnostic
}

func (c *Syncer) refuseEntry(ctx context.Context, id int64, entry string, failure entryFailure) error {
	object, cause := failure.object, failure.cause
	c.remember(id, entry, failure.status, cause)
	var suspensionError error
	if object != nil && publicerr.Permanent(cause) {
		suspensionError = c.suspend(ctx, object.Name)
	}
	return errors.Join(transientError(cause), suspensionError)
}
func (c *Syncer) refuseRepository(ctx context.Context, repo Repository, objects map[string]*wfv1.CronWorkflow, cause *publicerr.Error) error {
	id := fileInvalid
	switch cause.Code() {
	case publicerr.NotFound:
		id = branchMissing
	case publicerr.Unavailable:
		id = gitUnavailable
	}
	failure := &refusal{cause, id}
	c.refusals[repo.ID][""] = failure
	failures := []error{transientError(cause)}
	if publicerr.Permanent(cause) {
		for _, object := range objects {
			failures = append(failures, c.suspend(ctx, object.Name))
		}
	}
	return errors.Join(failures...)
}

func branchFailure(entry, branch string, cause error) (diagnostic, *publicerr.Error) {
	if !publicerr.Permanent(cause) {
		return gitUnavailable, problem(gitUnavailable, cause)
	}
	failure := problem(branchMissing, cause)
	label := "file"
	if entry != "" {
		label = fmt.Sprintf("entry %q", entry)
	}
	failure.Detail = fmt.Sprintf("%s: field %q has value %q: push this branch to the oberth remote or choose one already there.", label, "branch", branch)
	return branchMissing, failure
}
