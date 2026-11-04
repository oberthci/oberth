package service

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/oberthci/oberth/internal/model"
)

type issueTrailerStore interface {
	PendingIssueTrailerWork(context.Context) ([]model.IssueTrailerWork, error)
	CompleteIssueTrailerWork(context.Context, string, []model.CommitIssueTrailer) error
	SkipIssueTrailerWork(context.Context, string, string) error
}

type issueTrailerGit interface {
	GreenIssueTrailers(context.Context, string, string, string) (bool, []model.CommitIssueTrailer, error)
}

// Optional repository policy must not stop the scheduler or change the green
// publication verdict. The durable pending row supplies the retry obligation.
func (scheduler *Scheduler) retryIssueTrailers(ctx context.Context, require mutationRequirement) {
	if err := scheduler.recoverIssueTrailers(ctx, require); err != nil {
		log.Printf("issue trailer recovery pending: %v", err)
	}
}

func (scheduler *Scheduler) recoverIssueTrailers(ctx context.Context, require mutationRequirement) error {
	database, ok := scheduler.store.(issueTrailerStore)
	if !ok {
		return nil
	}
	git, ok := scheduler.git.(issueTrailerGit)
	if !ok {
		return nil
	}
	if err := require(ctx); err != nil {
		return err
	}
	pending, err := database.PendingIssueTrailerWork(ctx)
	if err != nil {
		return err
	}
	var failures error
	for _, work := range pending {
		err := func() error {
			repo, err := scheduler.database().Repository(ctx, work.RepoID)
			if err != nil {
				return err
			}
			upstream, err := scheduler.database().Upstream(ctx, repo.UpstreamID)
			if err != nil {
				return err
			}
			resume := suspendDispatch(ctx)
			enabled, changes, err := git.GreenIssueTrailers(ctx, upstream.QualifiedRepo(repo.Name), work.PreviousSHA, work.SHA)
			resume()
			if err != nil {
				if errors.Is(err, model.ErrIssueTrailerPermanent) {
					if gateErr := require(ctx); gateErr != nil {
						return gateErr
					}
					return database.SkipIssueTrailerWork(ctx, work.PublicationID, err.Error())
				}
				return fmt.Errorf("issue trailers for publication %s: %w", work.PublicationID, err)
			}
			if !enabled {
				changes = nil
			}
			if err := require(ctx); err != nil {
				return err
			}
			if err := database.CompleteIssueTrailerWork(ctx, work.PublicationID, changes); err != nil {
				return err
			}
			return nil
		}()
		failures = errors.Join(failures, err)
	}
	return failures
}

// Display metadata is best effort and cannot change the run or issue verdict.
func (scheduler *Scheduler) recordRunIssueTrailers(ctx context.Context, run model.Run, input string, require mutationRequirement) error {
	if run.IssueTrailersRecorded {
		return nil
	}
	git, ok := scheduler.git.(interface {
		CommitIssueTrailers(context.Context, string, string) ([]model.CommitIssueTrailer, error)
	})
	if !ok {
		return nil
	}
	database, ok := scheduler.store.(interface {
		RecordRunIssueTrailers(context.Context, string, string, []model.CommitIssueTrailer) error
	})
	if !ok {
		return nil
	}
	trailers, err := git.CommitIssueTrailers(ctx, input, run.SHA)
	if err != nil {
		log.Printf("run %s issue trailer display unavailable: %v", run.ID, err)
		return nil
	}
	if err := require(ctx); err != nil {
		return err
	}
	if err := database.RecordRunIssueTrailers(ctx, run.ID, run.SHA, trailers); err != nil {
		log.Printf("run %s issue trailer display not recorded: %v", run.ID, err)
	}
	return nil
}
