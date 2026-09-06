package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/schedule"
	"github.com/oberthci/oberth/internal/store"
)

func runSchedules(ctx context.Context, arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("schedules", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	databasePath := flags.String("database", "/data/oberth.sqlite", "SQLite database path (in-pod)")
	dataRoot := flags.String("data-root", "/data", "Oberth data root holding the git cache")
	minInterval := flags.Duration("min-interval", defaultScheduleMinInterval, "shortest interval a repository may schedule itself at")
	maxEntries := flags.Int("max-entries", defaultScheduleMaxEntries, "most entries one repository may declare")
	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			flags.SetOutput(output)
			flags.Usage()
			return nil
		}
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if flags.NArg() > 1 {
		return fmt.Errorf("%w: schedules [repository]", errUsage)
	}

	database, err := store.OpenAdminClient(ctx, *databasePath, store.Options{})
	if err != nil {
		return err
	}
	defer func() { _ = database.Close() }()
	repositories, err := database.ListRepositories(ctx)
	if err != nil {
		return err
	}
	cache, err := gitcache.New(gitcache.Config{
		Root:     filepath.Join(*dataRoot, "git"),
		Upstream: func(string) (string, error) { return "", errors.New("schedule reads never contact an upstream") },
	})
	if err != nil {
		return err
	}

	wanted := flags.Arg(0)
	limits := schedule.Limits{MinInterval: *minInterval, MaxEntries: *maxEntries}
	upstreams, err := database.ListUpstreams(ctx)
	if err != nil {
		return err
	}
	upstreamByID := make(map[int64]model.Upstream, len(upstreams))
	for _, upstream := range upstreams {
		upstreamByID[upstream.ID] = upstream
	}
	now := time.Now().UTC()
	printed := false
	for _, repository := range repositories {
		if wanted != "" && repository.Name != wanted {
			continue
		}
		upstream, ok := upstreamByID[repository.UpstreamID]
		if !ok {
			return fmt.Errorf("repository %s references unknown upstream id %d", repository.Name, repository.UpstreamID)
		}
		printed = true
		// Cache reads and schedule-outcome keys use the fully-qualified
		// name, matching the schedule engine's dedup keys (issue #264).
		if err := reportRepositorySchedule(ctx, output, cache, database, repository.Name, upstream.QualifiedRepo(repository.Name), repository.DefaultBranch, limits, now); err != nil {
			return err
		}
	}
	if !printed {
		_, err := fmt.Fprintf(output, "no repository named %q\n", wanted)
		return err
	}
	return nil
}

func reportRepositorySchedule(
	ctx context.Context, output io.Writer, cache *gitcache.Cache, database *store.Store,
	repo, qualifiedRepo, branch string, limits schedule.Limits, now time.Time,
) error {
	sha, err := cache.RefSHA(ctx, qualifiedRepo, branch)
	if err != nil {
		return nil
	}
	raw, err := cache.ReadBlob(ctx, qualifiedRepo, sha, schedule.FileName, schedule.MaxSourceBytes)
	if err != nil {
		return nil
	}
	if _, err := fmt.Fprintf(output, "%s (%s)\n", repo, branch); err != nil {
		return err
	}
	file, err := schedule.Parse(raw, limits, now)
	if err != nil {
		_, printErr := fmt.Fprintf(output, "  refused: %v\n", err)
		return printErr
	}
	outcomes, _ := database.ScheduleOutcomes(ctx, qualifiedRepo)
	last := map[string]store.ScheduleOutcome{}
	for _, outcome := range outcomes {
		last[outcome.Entry] = outcome
	}
	for _, entry := range file.Entries {
		next, nextErr := entry.Next(now)
		when := "unknown"
		if nextErr == nil {
			when = next.Format("2006-01-02 15:04") + "Z"
		}
		status := "never run"
		if outcome, ok := last[entry.Name]; ok {
			status = outcome.Outcome + " " + outcome.FiredAt.Format("2006-01-02 15:04") + "Z"
		}
		if _, err := fmt.Fprintf(output, "  %-20s %-16s next %s  last %s\n",
			entry.Name, entry.Cron, when, status); err != nil {
			return err
		}
	}
	for _, refusal := range file.Refused {
		if _, err := fmt.Fprintf(output, "  %-20s REFUSED: %s\n", refusal.Name, refusal.Reason); err != nil {
			return err
		}
	}
	return nil
}
