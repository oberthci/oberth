package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/schedule"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/store"
)

const ScheduleActor = "oberth:scheduler"

const (
	schedulePerRepoTimeout = 30 * time.Second
	scheduleTickInterval   = time.Minute
	scheduleRunLookback    = 5
)

func isServerActor(actor string) bool { return actor == ScheduleActor }

type ScheduleGit interface {
	RefSHA(ctx context.Context, repo, branch string) (string, error)
	ReadBlob(ctx context.Context, repo, sha, file string, limit int) ([]byte, error)
}

type ScheduleEnqueuer interface {
	EnqueueCI(ctx context.Context, request service.CIRequest) (model.EnqueueRunResult, error)
	EnqueueScheduledCI(context.Context, service.CIRequest, model.ScheduleFireSpec) (model.ScheduleEnqueueResult, error)
}

type ScheduleRuns interface {
	ListRecentRuns(ctx context.Context, filter model.RunListFilter) ([]model.Run, error)
}

type ScheduleState interface {
	// A present zero cursor means a known-due entry has not been consumed by firing or coalescing.
	// An absent cursor starts from this server's initial observation instead.
	ScheduleFires(ctx context.Context, repo string) (map[string]time.Time, error)
	RecordScheduleFire(ctx context.Context, repo, entry string, at time.Time, outcome string) error
}

// SchedulePauseReader is an optional ScheduleState capability: the paused
// entries of one repository (#1106). A state without it never pauses.
type SchedulePauseReader interface {
	SchedulePauses(ctx context.Context, repo string) (map[string]model.SchedulePause, error)
}

// ScheduleOutcomeReader is an optional ScheduleState capability: the last
// recorded outcome of every entry of one repository.
type ScheduleOutcomeReader interface {
	ScheduleOutcomes(ctx context.Context, repo string) ([]store.ScheduleOutcome, error)
}

// ScheduleUpstreamResolver provides upstream identity for schedule-fire
// key construction. The qualified repo name is the key in schedule_fires
// so same-name repos under different upstreams have independent schedules.
type ScheduleUpstreamResolver interface {
	Upstream(context.Context, int64) (model.Upstream, error)
}

type SchedulesConfig struct {
	Repositories func(context.Context) ([]model.Repository, error)
	Git          ScheduleGit
	Runs         ScheduleRuns
	Enqueuer     ScheduleEnqueuer
	State        ScheduleState
	Upstreams    ScheduleUpstreamResolver
	MinInterval  time.Duration
	MaxEntries   int
	Logger       *log.Logger
}

type Schedules struct {
	config   SchedulesConfig
	observed time.Time
}

func NewSchedules(config SchedulesConfig) *Schedules {
	return &Schedules{config: config, observed: time.Now().UTC()}
}

// qualifiedRepoName constructs the "upstream/org/repo" key for schedule_fires.
//
// A resolver failure returns an error and the caller skips the whole tick
// rather than falling back to the bare name: a bare-keyed record is invisible
// to every later qualified read, so the entry's "already fired" dedup would
// miss and the schedule would fire again. Skipping a tick only delays; a
// mixed-key write corrupts the dedup state. A nil resolver (fixtures without
// upstream wiring) keeps the bare name, which is then also the read key.
func (schedules *Schedules) qualifiedRepoName(ctx context.Context, repository model.Repository) (string, error) {
	if schedules.config.Upstreams == nil {
		return repository.Name, nil
	}
	upstream, err := schedules.config.Upstreams.Upstream(ctx, repository.UpstreamID)
	if err != nil {
		return "", err
	}
	return upstream.QualifiedRepo(repository.Name), nil
}

func (schedules *Schedules) Run(ctx context.Context) error {
	ticker := time.NewTicker(scheduleTickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case now := <-ticker.C:
			schedules.Tick(ctx, now.UTC())
		}
	}
}

func (schedules *Schedules) Tick(ctx context.Context, now time.Time) {
	if schedules.config.Repositories == nil || schedules.config.Git == nil || schedules.config.Enqueuer == nil {
		return
	}
	repositories, err := schedules.config.Repositories(ctx)
	if err != nil {
		return
	}
	for _, repository := range repositories {
		schedules.tickRepository(ctx, repository, now)
	}
}

func (schedules *Schedules) tickRepository(ctx context.Context, repository model.Repository, now time.Time) {
	if repository.ArchivedAt != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, schedulePerRepoTimeout)
	defer cancel()

	branch := strings.TrimSpace(repository.DefaultBranch)
	if branch == "" {
		return
	}
	qualifiedName, err := schedules.qualifiedRepoName(ctx, repository)
	if err != nil {
		// Skip the tick: recording under a fallback key would corrupt the
		// fire dedup state (see qualifiedRepoName). The next tick retries.
		return
	}
	sha, err := schedules.config.Git.RefSHA(ctx, qualifiedName, branch)
	if err != nil {
		return
	}
	raw, err := schedules.config.Git.ReadBlob(ctx, qualifiedName, sha, schedule.FileName, schedule.MaxSourceBytes)
	if err != nil {
		return
	}
	file, err := schedule.Parse(raw, schedule.Limits{
		MinInterval: schedules.config.MinInterval, MaxEntries: schedules.config.MaxEntries,
	}, schedules.observed)
	if err != nil {
		schedules.record(ctx, qualifiedName, "", now, "refused")
		return
	}
	for _, refusal := range file.Refused {
		schedules.record(ctx, qualifiedName, refusal.Name, now, "refused")
	}

	if schedules.config.State == nil {
		schedules.logFailure(qualifiedName, "", errors.New("schedule state is unavailable"))
		return
	}
	last, err := schedules.config.State.ScheduleFires(ctx, qualifiedName)
	if err != nil {
		schedules.logFailure(qualifiedName, "", err)
		for _, entry := range file.Entries {
			// This is an observation failure, not proof the entry is due.
			// Do not invent a pending cursor for a future occurrence.
			schedules.record(ctx, qualifiedName, entry.Name, now, "failed_state")
		}
		return
	}
	due := file.Due(last, now)
	if len(due) == 0 {
		return
	}
	due = schedules.withoutPaused(ctx, qualifiedName, due, now)
	if len(due) == 0 {
		return
	}
	// Queue due work even while this repository is busy. Resource admission
	// owns execution capacity; waiting for an idle repository here can starve
	// a nightly behind a continuous stream of ordinary branch runs.
	for _, entry := range due {
		previous, seen := last[entry.Name]
		schedules.fire(ctx, repository, qualifiedName, entry, sha, branch, model.ScheduleFireSpec{
			Repo: qualifiedName, At: now, Previous: previous, HasPrevious: seen,
		})
	}
}

func (schedules *Schedules) fire(
	ctx context.Context, repository model.Repository, qualifiedName string, entry schedule.Entry, sha, defaultBranch string, fire model.ScheduleFireSpec,
) {
	branch, target, err := schedules.entryTarget(ctx, qualifiedName, entry, sha, defaultBranch)
	if err == nil {
		_, err = schedules.config.Enqueuer.EnqueueScheduledCI(ctx, service.CIRequest{
			EventID: scheduleEventID(), Repository: repository, Branch: branch,
			SHA: target, Actor: ScheduleActor, Trigger: "schedule", ScheduleEntry: entry.Name,
		}, fire)
	}
	if err != nil {
		schedules.logFailure(qualifiedName, entry.Name, err)
		schedules.record(ctx, qualifiedName, entry.Name, fire.At, "failed")
	}
	// The atomic enqueue owns fired/coalesced cursor writes. In particular,
	// never overwrite its committed outcome with a separate successful write.
}

// entryTarget is the branch an entry tests and that branch's head: the default
// branch head already read for the tick, or the entry's own branch.
func (schedules *Schedules) entryTarget(ctx context.Context, qualifiedName string, entry schedule.Entry, defaultHead, defaultBranch string) (branch, sha string, err error) {
	branch = entry.Branch
	if branch == "" {
		branch = defaultBranch
	}
	if branch == defaultBranch {
		return branch, defaultHead, nil
	}
	sha, err = schedules.config.Git.RefSHA(ctx, qualifiedName, branch)
	return branch, sha, err
}

// enqueue is the manual schedule admission path: EnqueueCI with trigger
// "schedule". Cron uses EnqueueScheduledCI for atomic cursor consumption.
func (schedules *Schedules) enqueue(ctx context.Context, repository model.Repository, branch, sha, entry, actor string) (model.EnqueueRunResult, error) {
	return schedules.config.Enqueuer.EnqueueCI(ctx, service.CIRequest{
		EventID:       scheduleEventID(),
		Repository:    repository,
		Branch:        branch,
		SHA:           sha,
		Actor:         actor,
		Trigger:       "schedule",
		ScheduleEntry: entry,
	})
}

// withoutPaused drops the paused entries from a due list, recording outcome
// "paused" for each, and returns the rest. A pause that cannot be read skips
// the repository's tick (the next one retries) rather than firing what an
// operator may have held.
func (schedules *Schedules) withoutPaused(ctx context.Context, qualifiedName string, due []schedule.Entry, now time.Time) []schedule.Entry {
	reader, ok := schedules.config.State.(SchedulePauseReader)
	if !ok {
		return due
	}
	pauses, err := reader.SchedulePauses(ctx, qualifiedName)
	if err != nil {
		schedules.logFailure(qualifiedName, "", err)
		return nil
	}
	var running []schedule.Entry
	for _, entry := range due {
		if _, paused := pauses[entry.Name]; paused {
			schedules.record(ctx, qualifiedName, entry.Name, now, "paused")
			continue
		}
		running = append(running, entry)
	}
	return running
}

func (schedules *Schedules) busy(ctx context.Context, repoID int64) bool {
	if schedules.config.Runs == nil {
		return false
	}
	runs, err := schedules.config.Runs.ListRecentRuns(ctx, model.RunListFilter{RepoID: repoID, Limit: scheduleRunLookback})
	if err != nil {
		return true
	}
	for _, run := range runs {
		if run.Status.Active() {
			return true
		}
	}
	return false
}

func (schedules *Schedules) record(ctx context.Context, repo, entry string, at time.Time, outcome string) {
	if schedules.config.State == nil {
		return
	}
	if err := schedules.config.State.RecordScheduleFire(ctx, repo, entry, at, outcome); err != nil {
		schedules.logFailure(repo, entry, err)
	}
}

func (schedules *Schedules) logFailure(repo, entry string, err error) {
	logger := schedules.config.Logger
	if logger == nil {
		logger = log.Default()
	}
	logger.Printf("schedule tick failed repo=%q entry=%q: %v", repo, entry, err)
}

func scheduleEventID() string {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "schedule-" + time.Now().UTC().Format("20060102150405.000000000")
	}
	return "schedule-" + hex.EncodeToString(buffer)
}

type MemoryScheduleState struct {
	mu    sync.Mutex
	fires map[string]map[string]time.Time
}

func NewMemoryScheduleState() *MemoryScheduleState {
	return &MemoryScheduleState{fires: map[string]map[string]time.Time{}}
}

func (state *MemoryScheduleState) ScheduleFires(_ context.Context, repo string) (map[string]time.Time, error) {
	state.mu.Lock()
	defer state.mu.Unlock()
	copied := map[string]time.Time{}
	for name, when := range state.fires[repo] {
		copied[name] = when
	}
	return copied, nil
}

func (state *MemoryScheduleState) RecordScheduleFire(_ context.Context, repo, entry string, at time.Time, outcome string) error {
	if entry == "" || (outcome != "fired" && outcome != "coalesced" && outcome != "skipped" && outcome != "failed" && outcome != "paused") {
		return nil
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.fires[repo] == nil {
		state.fires[repo] = map[string]time.Time{}
	}
	if outcome == "fired" || outcome == "coalesced" {
		state.fires[repo][entry] = at.UTC()
	} else if _, seen := state.fires[repo][entry]; !seen {
		state.fires[repo][entry] = time.Time{}
	}
	return nil
}
