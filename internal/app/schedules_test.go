package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
)

type fakeScheduleGit struct {
	sha   map[string]string
	blob  map[string]string
	fails map[string]error
}

func (f *fakeScheduleGit) RefSHA(_ context.Context, repo, _ string) (string, error) {
	if err := f.fails[repo]; err != nil {
		return "", err
	}
	sha, ok := f.sha[repo]
	if !ok {
		return "", errors.New("no such branch")
	}
	return sha, nil
}

func (f *fakeScheduleGit) ReadBlob(_ context.Context, repo, _, _ string, _ int) ([]byte, error) {
	body, ok := f.blob[repo]
	if !ok {
		return nil, errors.New("no schedule file")
	}
	return []byte(body), nil
}

type fakeEnqueuer struct {
	requests []service.CIRequest
	err      error
}

func (f *fakeEnqueuer) EnqueueCI(_ context.Context, request service.CIRequest) (model.EnqueueRunResult, error) {
	f.requests = append(f.requests, request)
	return model.EnqueueRunResult{}, f.err
}

type fakeRunLister struct {
	active map[int64]bool
}

func (f *fakeRunLister) ListRecentRuns(_ context.Context, filter model.RunListFilter) ([]model.Run, error) {
	if f.active[filter.RepoID] {
		return []model.Run{{Status: model.RunRunning}}, nil
	}
	return []model.Run{{Status: model.RunPassed}}, nil
}

type fakeScheduleState struct {
	fires  map[string]map[string]time.Time
	events []string
}

func (f *fakeScheduleState) ScheduleFires(_ context.Context, repo string) (map[string]time.Time, error) {
	return f.fires[repo], nil
}

func (f *fakeScheduleState) RecordScheduleFire(_ context.Context, repo, entry string, _ time.Time, outcome string) error {
	f.events = append(f.events, repo+"/"+entry+":"+outcome)
	if f.fires == nil {
		f.fires = map[string]map[string]time.Time{}
	}
	return nil
}

const nightlyFile = "schedules:\n  - name: nightly\n    cron: \"0 3 * * *\"\n"

func testSchedules(t *testing.T, git *fakeScheduleGit, repos []model.Repository) (*Schedules, *fakeEnqueuer, *fakeRunLister, *fakeScheduleState) {
	t.Helper()
	enqueuer := &fakeEnqueuer{}
	runs := &fakeRunLister{active: map[int64]bool{}}
	state := &fakeScheduleState{fires: map[string]map[string]time.Time{}}
	schedules := NewSchedules(SchedulesConfig{
		Repositories: func(context.Context) ([]model.Repository, error) { return repos, nil },
		Git:          git,
		Runs:         runs,
		Enqueuer:     enqueuer,
		State:        state,
		MinInterval:  15 * time.Minute,
		MaxEntries:   4,
	})
	return schedules, enqueuer, runs, state
}

func oneRepo() []model.Repository {
	return []model.Repository{{ID: 1, Name: "alpha", DefaultBranch: "main"}}
}

func instant(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed.UTC()
}

func TestATickEnqueuesADueScheduleAsAServerActor(t *testing.T) {
	t.Parallel()
	git := &fakeScheduleGit{
		sha:  map[string]string{"alpha": "0123456789abcdef0123456789abcdef01234567"},
		blob: map[string]string{"alpha": nightlyFile},
	}
	schedules, enqueuer, _, _ := testSchedules(t, git, oneRepo())
	schedules.observed = instant(t, "2026-08-24T00:00:00Z")

	schedules.Tick(context.Background(), instant(t, "2026-08-24T03:05:00Z"))

	if len(enqueuer.requests) != 1 {
		t.Fatalf("enqueued %d runs, want 1", len(enqueuer.requests))
	}
	request := enqueuer.requests[0]
	if request.Trigger != "schedule" {
		t.Fatalf("trigger = %q, want schedule", request.Trigger)
	}
	if request.Actor == "" {
		t.Fatal("no actor; EnqueueCI would refuse this")
	}
	if request.Actor == "alpha" || !isServerActor(request.Actor) {
		t.Fatalf("actor %q is not a distinct server identity", request.Actor)
	}
	if request.SHA != "0123456789abcdef0123456789abcdef01234567" {
		t.Fatalf("SHA = %q", request.SHA)
	}
	if request.Branch != "main" {
		t.Fatalf("branch = %q, want the repository default", request.Branch)
	}
	if request.EventID == "" {
		t.Fatal("no event ID; EnqueueCI would refuse this")
	}
}

func TestATickFiresEvenWhenTheCommitHasNotChanged(t *testing.T) {
	t.Parallel()
	git := &fakeScheduleGit{
		sha:  map[string]string{"alpha": "0123456789abcdef0123456789abcdef01234567"},
		blob: map[string]string{"alpha": nightlyFile},
	}
	schedules, enqueuer, _, state := testSchedules(t, git, oneRepo())
	schedules.observed = instant(t, "2026-08-24T00:00:00Z")

	schedules.Tick(context.Background(), instant(t, "2026-08-24T03:05:00Z"))
	state.fires["alpha"] = map[string]time.Time{"nightly": instant(t, "2026-08-24T03:00:00Z")}
	schedules.Tick(context.Background(), instant(t, "2026-08-25T03:05:00Z"))

	if len(enqueuer.requests) != 2 {
		t.Fatalf("enqueued %d runs across two nights, want 2", len(enqueuer.requests))
	}
	if enqueuer.requests[0].SHA != enqueuer.requests[1].SHA {
		t.Fatal("the fixture changed SHA; this test is meant to prove an unchanged one still runs")
	}
}

func TestATickSkipsARepositoryThatIsAlreadyBusyAndRecordsWhy(t *testing.T) {
	t.Parallel()
	git := &fakeScheduleGit{
		sha:  map[string]string{"alpha": "0123456789abcdef0123456789abcdef01234567"},
		blob: map[string]string{"alpha": nightlyFile},
	}
	schedules, enqueuer, runs, state := testSchedules(t, git, oneRepo())
	schedules.observed = instant(t, "2026-08-24T00:00:00Z")
	runs.active[1] = true

	schedules.Tick(context.Background(), instant(t, "2026-08-24T03:05:00Z"))

	if len(enqueuer.requests) != 0 {
		t.Fatal("a fire was queued behind a run that was already going")
	}
	if len(state.events) == 0 {
		t.Fatal("the skip was silent")
	}
	if state.events[0] != "alpha/nightly:skipped" {
		t.Fatalf("recorded %q, want a skip naming the entry", state.events[0])
	}
}

func TestABrokenRepositoryDoesNotStopAHealthyOne(t *testing.T) {
	t.Parallel()
	git := &fakeScheduleGit{
		sha:   map[string]string{"alpha": "0123456789abcdef0123456789abcdef01234567", "beta": "0123456789abcdef0123456789abcdef01234567"},
		blob:  map[string]string{"alpha": "schedules: [", "beta": nightlyFile},
		fails: map[string]error{},
	}
	repos := []model.Repository{
		{ID: 1, Name: "alpha", DefaultBranch: "main"},
		{ID: 2, Name: "beta", DefaultBranch: "main"},
	}
	schedules, enqueuer, _, state := testSchedules(t, git, repos)
	schedules.observed = instant(t, "2026-08-24T00:00:00Z")

	schedules.Tick(context.Background(), instant(t, "2026-08-24T03:05:00Z"))

	if len(enqueuer.requests) != 1 || enqueuer.requests[0].Repository.Name != "beta" {
		t.Fatalf("a malformed file in alpha stopped beta: %+v", enqueuer.requests)
	}
	found := false
	for _, event := range state.events {
		if event == "alpha/:refused" {
			found = true
		}
	}
	if !found {
		t.Fatalf("alpha's refusal was not recorded: %v", state.events)
	}
}

func TestAnUnreadableBranchDoesNotStopAHealthyRepository(t *testing.T) {
	t.Parallel()
	git := &fakeScheduleGit{
		sha:   map[string]string{"beta": "0123456789abcdef0123456789abcdef01234567"},
		blob:  map[string]string{"beta": nightlyFile},
		fails: map[string]error{"alpha": errors.New("branch is gone")},
	}
	repos := []model.Repository{
		{ID: 1, Name: "alpha", DefaultBranch: "main"},
		{ID: 2, Name: "beta", DefaultBranch: "main"},
	}
	schedules, enqueuer, _, _ := testSchedules(t, git, repos)
	schedules.observed = instant(t, "2026-08-24T00:00:00Z")

	schedules.Tick(context.Background(), instant(t, "2026-08-24T03:05:00Z"))

	if len(enqueuer.requests) != 1 || enqueuer.requests[0].Repository.Name != "beta" {
		t.Fatalf("an unreadable branch in alpha stopped beta: %+v", enqueuer.requests)
	}
}

func TestARepositoryWithNoScheduleFileIsUntouched(t *testing.T) {
	t.Parallel()
	git := &fakeScheduleGit{
		sha:  map[string]string{"alpha": "0123456789abcdef0123456789abcdef01234567"},
		blob: map[string]string{},
	}
	schedules, enqueuer, _, state := testSchedules(t, git, oneRepo())
	schedules.observed = instant(t, "2026-08-24T00:00:00Z")

	schedules.Tick(context.Background(), instant(t, "2026-08-24T03:05:00Z"))

	if len(enqueuer.requests) != 0 {
		t.Fatal("a repository with no schedule file ran something")
	}
	if len(state.events) != 0 {
		t.Fatalf("a repository with no schedule file recorded %v", state.events)
	}
}

func TestAFailedEnqueueIsRecordedRatherThanLost(t *testing.T) {
	t.Parallel()
	git := &fakeScheduleGit{
		sha:  map[string]string{"alpha": "0123456789abcdef0123456789abcdef01234567"},
		blob: map[string]string{"alpha": nightlyFile},
	}
	schedules, enqueuer, _, state := testSchedules(t, git, oneRepo())
	schedules.observed = instant(t, "2026-08-24T00:00:00Z")
	enqueuer.err = errors.New("queue is closed")

	schedules.Tick(context.Background(), instant(t, "2026-08-24T03:05:00Z"))

	if len(state.events) == 0 || state.events[0] != "alpha/nightly:failed" {
		t.Fatalf("a failed enqueue recorded %v", state.events)
	}
}

func TestARestartAfterAMissedFireRunsOnceNotOncePerMissedFire(t *testing.T) {
	t.Parallel()
	git := &fakeScheduleGit{
		sha:  map[string]string{"alpha": "0123456789abcdef0123456789abcdef01234567"},
		blob: map[string]string{"alpha": nightlyFile},
	}
	schedules, enqueuer, _, state := testSchedules(t, git, oneRepo())
	schedules.observed = instant(t, "2026-08-01T00:00:00Z")
	state.fires["alpha"] = map[string]time.Time{"nightly": instant(t, "2026-08-01T03:00:00Z")}

	schedules.Tick(context.Background(), instant(t, "2026-08-24T09:00:00Z"))

	if len(enqueuer.requests) != 1 {
		t.Fatalf("23 days of downtime enqueued %d runs, want exactly 1", len(enqueuer.requests))
	}
}

func TestAFireIsNotRepeatedWithinTheSameMinute(t *testing.T) {
	t.Parallel()
	git := &fakeScheduleGit{
		sha:  map[string]string{"alpha": "0123456789abcdef0123456789abcdef01234567"},
		blob: map[string]string{"alpha": nightlyFile},
	}
	schedules, enqueuer, _, state := testSchedules(t, git, oneRepo())
	schedules.observed = instant(t, "2026-08-24T00:00:00Z")

	moment := instant(t, "2026-08-24T03:05:00Z")
	schedules.Tick(context.Background(), moment)
	state.fires["alpha"] = map[string]time.Time{"nightly": moment}
	schedules.Tick(context.Background(), moment)

	if len(enqueuer.requests) != 1 {
		t.Fatalf("two ticks in the same minute enqueued %d runs, want 1", len(enqueuer.requests))
	}
}

func TestTheMemoryStateOnlyAdvancesOnAFiredOutcome(t *testing.T) {
	t.Parallel()
	state := NewMemoryScheduleState()
	ctx := context.Background()
	when := instant(t, "2026-08-24T03:00:00Z")
	for _, outcome := range []string{"skipped", "refused", "failed"} {
		if err := state.RecordScheduleFire(ctx, "alpha", "nightly", when, outcome); err != nil {
			t.Fatal(err)
		}
	}
	fires, err := state.ScheduleFires(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if len(fires) != 0 {
		t.Fatalf("a non-fired outcome advanced the clock: %+v", fires)
	}
}
