package api_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/logcontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

// TestUIExampleServer serves fixed examples only when explicitly invoked.
// OBERTH_UI_EXAMPLE=127.0.0.1:8099 go test ./internal/api -run '^TestUIExampleServer$' -v
func TestUIExampleServer(t *testing.T) {
	address := os.Getenv("OBERTH_UI_EXAMPLE")
	if address == "" {
		t.Skip("set OBERTH_UI_EXAMPLE to serve fixed examples")
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	f := uiExample(t)
	server := &http.Server{Handler: f.server.Handler(), ReadHeaderTimeout: 5 * time.Second}
	ctx, stop := signal.NotifyContext(t.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() { <-ctx.Done(); _ = server.Close() }()
	t.Log("Fixed examples at http://" + listener.Addr().String() + "/runs")
	if err := server.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
		t.Fatal(err)
	}
}

func uiExample(t *testing.T) *fixture {
	t.Helper()
	at := time.Date(2026, 11, 16, 14, 0, 0, 0, time.UTC)
	end := at.Add(48 * time.Second)
	sha := "1234567890abcdef1234567890abcdef12345678"
	steps := []stepmodel.RunStep{
		{Path: "z-checkout", Name: "checkout", Status: stepmodel.StepPassed, StartedAt: &at, FinishedAt: &end},
		{Path: "y-build", Name: "install-release-support-and-golangci-lint", Status: stepmodel.StepPassed, CredentialIntent: true, StartedAt: &at, FinishedAt: &end},
		{Path: "x-test", Name: "test", ParallelGroup: 1, Status: stepmodel.StepFailed, StartedAt: &at, FinishedAt: &end, AttemptCount: 2, DisplayReason: "test failed: exit 2", Detail: "The test command exited with code 2.", Attempts: []stepmodel.StepAttempt{{NodeID: "attempt-one", Status: stepmodel.StepFailed}, {NodeID: "attempt-two", Status: stepmodel.StepFailed}}, Logs: []stepmodel.LogSelector{{NodeID: "attempt-two", Container: "main"}}},
		{Path: "w-publish", Name: "publish", ParallelGroup: 1, Status: stepmodel.StepPassed, StartedAt: &at, FinishedAt: &end},
	}
	runs := []apicontract.RunRecord{
		{RunSummary: stepmodel.RunSummary{ID: "11111111", Repo: "origin/team/api", Ref: "main", Status: stepmodel.RunPassed, CreatedAt: at, StartedAt: &at, FinishedAt: &end, CommitSHA: &sha, ObjectSHA: sha}, RunCause: model.RunCause{Actor: "owner", Trigger: model.TriggerPromotion}},
		{RunSummary: stepmodel.RunSummary{ID: "22222222", Repo: "origin/team/api", Ref: "fix-login", Status: stepmodel.RunFailed, DisplayReason: "test failed: exit 2", CreatedAt: at, StartedAt: &at, FinishedAt: &end, CommitSHA: &sha, ObjectSHA: sha, Failure: &stepmodel.FailureRecord{Step: "x-test", Name: "test", NodeID: "attempt-two"}}, RunCause: model.RunCause{Actor: "worker", Trigger: model.TriggerBranch, AssistRequested: true}},
		{RunSummary: stepmodel.RunSummary{ID: "33333333", Repo: "origin/team/web", Ref: "feature/search", Status: stepmodel.RunRunning, DisplayReason: "waiting for an available build node soon", CreatedAt: at, StartedAt: &at, CommitSHA: &sha, ObjectSHA: sha}, RunCause: model.RunCause{Actor: "oberth:scheduler", Trigger: model.TriggerSchedule, ScheduleEntry: "nightly"}},
		{RunSummary: stepmodel.RunSummary{ID: "44444444", Repo: "mirror/tools/cli", Ref: "main", Status: stepmodel.RunQueued, DisplayReason: "waiting for execution", CreatedAt: at, ObjectSHA: sha}, RunCause: model.RunCause{Actor: "worker", Trigger: model.TriggerBranch, RerunOf: strings.Repeat("22222222", 4)}},
	}
	for index := range runs {
		runs[index].ID = strings.Repeat(runs[index].ID, 4)
		runs[index].Steps = steps
		runs[index].LogicalStepCounts = stepmodel.LogicalStepCounts{Total: 4, Exact: true, Passed: 3, Failed: 1}
	}
	return setup(t, func(c *api.Config, b *apicontract.Bindings) {
		c.Runtime.Now = testNow
		if token := os.Getenv("OBERTH_UI_TOKEN"); token != "" {
			c.Auth = exampleAuth{Authenticator: c.Auth, token: token}
		}
		b.RunList.Handler = exampleRunList(runs)
		b.RunGet.Handler = apicontract.HandlerFunc[apicontract.RunGetArgs, apicontract.RunRecord](func(_ context.Context, _ apicontract.Caller, args apicontract.RunGetArgs) (apicontract.Call[apicontract.RunRecord], error) {
			return func(context.Context) (apicontract.RunRecord, error) {
				for _, run := range runs {
					if strings.HasPrefix(run.ID, args.ID) {
						return run, nil
					}
				}
				fallback := runs[1]
				fallback.ID = args.ID
				if args.ID == "77777777" {
					fallback = runs[0]
					fallback.ID = args.ID
					fallback.Ref = "feature/pass"
				}
				return fallback, nil
			}, nil
		})
		b.RepoList.Handler = &fake.Handler[apicontract.RepoListArgs, apicontract.RepoPage]{Value: apicontract.RepoPage{Repositories: exampleRepositories(runs, at)}}
		b.RunLogs.Handler = &fake.Handler[apicontract.RunLogsArgs, apicontract.LogResult]{Value: apicontract.LogResult{Content: "Running the authored test command\nFAIL pkg/check 0.40s\nexpected status 200, received 500\n<script>this is log text</script>\n", Meta: logcontract.Meta{ScanComplete: true, BytesExact: true}}}
		b.RunRerun.Handler = &fake.Handler[apicontract.RunRerunArgs, apicontract.Receipt]{Value: apicontract.Receipt{ID: "55555555"}}
		refused := publicerr.New(publicerr.Conflict, "This run cannot be cancelled.", "Read the current run.", nil)
		refused.Detail = "complete server detail"
		b.RunCancel.Handler = &fake.Handler[apicontract.RunCancelArgs, apicontract.Receipt]{Error: refused}
		b.RunHoldList.Handler = exampleHoldList(end)
		b.EventsWait.Handler = apicontract.HandlerFunc[apicontract.EventsWaitArgs, apicontract.EventResult](func(_ context.Context, _ apicontract.Caller, _ apicontract.EventsWaitArgs) (apicontract.Call[apicontract.EventResult], error) {
			return func(ctx context.Context) (apicontract.EventResult, error) {
				<-ctx.Done()
				return apicontract.EventResult{TimedOut: true}, ctx.Err()
			}, nil
		})
		if os.Getenv("OBERTH_UI_EVENTS") != "" {
			b.EventsWait.Handler = exampleEventsEverySecond(runs[2].ID)
		}
		issue := model.Issue{ID: 42, Title: "Flaky test in pkg/check", Body: "The check sometimes returns a different status.\n\nExpected: status 200.\nObserved: status 500.\n\nReproduce with the authored test command, then inspect its complete output.", State: model.IssueOpen, Kind: model.IssueManual, WorkStatus: model.IssueActive, Upstream: "origin", Repo: runs[0].Repo, Branch: "fix-login", Occurrences: 4, Stale: true, CreatedAt: at, UpdatedAt: end, Lock: &model.IssueLock{Owner: "editor", ExpiresAt: browserNow().Add(5 * time.Minute)}}
		issue.Imported = &model.IssueImportSource{SetSHA256: strings.Repeat("a", 64), Count: 1}
		issue.Author, issue.Closer = "source author", "source closer"
		issue.SourceClosedAt = new(at.Add(-time.Hour))
		issue.History = []model.IssueHistory{
			{Kind: "append", At: at, Author: "first source editor", Text: "First continued text <script>plain source</script>"},
			{Kind: "comment", At: at.Add(-time.Hour), Author: "second source editor", Text: "Second source entry"},
		}
		issues := exampleIssues()
		b.IssueGet.Handler = exampleImportedIssue(issue, issues)
		b.IssueList.Handler = exampleIssueList(issues)
		c.Views.Counts.Handler = &fake.Handler[apicontract.Empty, apicontract.RunCounts]{Value: apicontract.RunCounts{Queued: 1, Running: 2, Passed: 140, Failed: 9, Cancelled: 3}}
		b.SystemStatus.Handler = &fake.Handler[apicontract.SystemStatusArgs, apicontract.SystemStatus]{Value: apicontract.SystemStatus{Ready: true, Version: "1"}}
		c.Views.Containers.Handler = &fake.Handler[apicontract.IDArgs, apicontract.ContainerPage]{Value: apicontract.ContainerPage{Containers: []apicontract.Container{{NodeID: "attempt-one", Container: "main"}, {NodeID: "attempt-two", Container: "main"}}}}
	})
}

// exampleRepositories shapes the repository list as the server stores it: full
// refs, a latest run from an earlier day on a branch of ordinary length, and a
// repository whose default branch could not be read.
func exampleRepositories(runs []apicontract.RunRecord, at time.Time) []apicontract.Repository {
	api, web := runs[1].RunSummary, runs[2].RunSummary
	api.Ref, api.CreatedAt = "refs/heads/w5/own-m3", at.Add(-24*time.Hour)
	web.Ref = "refs/heads/feature/search"
	return []apicontract.Repository{
		{Repo: runs[0].Repo, DefaultBranch: "refs/heads/main", BranchCount: new(3), LatestRun: &api, RecentRuns: exampleMarks(at, "pfpppcfppppfrppppqfp")},
		{Repo: runs[2].Repo, DefaultBranch: "refs/heads/main", LatestRun: &web, RecentRuns: exampleMarks(at, "ppfpr")},
		{Repo: runs[3].Repo},
	}
}

// exampleMarks spells a repository strip oldest first, one letter per run:
// p passed, f failed, r running, q queued, c cancelled.
func exampleMarks(at time.Time, spelled string) []stateport.RunMark {
	statuses := map[rune]stepmodel.RunStatus{'p': stepmodel.RunPassed, 'f': stepmodel.RunFailed, 'r': stepmodel.RunRunning, 'q': stepmodel.RunQueued, 'c': stepmodel.RunCancelled}
	marks := []stateport.RunMark{}
	for i, letter := range spelled {
		marks = append(marks, stateport.RunMark{ID: fmt.Sprintf("%032x", 0x7700+i), Status: statuses[letter], Ref: "refs/heads/topic-" + fmt.Sprint(i), CreatedAt: at.Add(time.Duration(i-len(spelled)) * time.Hour)})
	}
	return marks
}

type exampleAuth struct {
	api.Authenticator
	token string
}

func (a exampleAuth) Bearer(ctx context.Context, token string) (apicontract.Caller, error) {
	if token == a.token {
		token = "valid"
	}
	return a.Authenticator.Bearer(ctx, token)
}

func exampleHoldList(at time.Time) apicontract.HandlerFunc[apicontract.RunHoldListArgs, apicontract.HoldPage] {
	return func(_ context.Context, _ apicontract.Caller, q apicontract.RunHoldListArgs) (apicontract.Call[apicontract.HoldPage], error) {
		return func(context.Context) (apicontract.HoldPage, error) {
			rows := []model.LiveHold{}
			if strings.HasPrefix(q.ID, "33333333") || strings.HasPrefix(q.ID, "22222222") {
				rows = append(rows, model.LiveHold{HeldNotice: model.HeldNotice{RunID: q.ID, Step: "test", NodeID: "node", PodUID: "pod-uid", HeldSince: at.Add(-time.Hour), TTLSeconds: 1200}, PodName: "held-pod", RemainingMS: 600000, AvailabilityUnknown: strings.HasPrefix(q.ID, "22222222")})
			}
			return apicontract.HoldPage{Holds: rows}, nil
		}, nil
	}
}

// exampleRunList narrows the fixed runs to the requested upstreams, as the
// server does before paging, so the browser check sees server-side filtering.
func exampleRunList(runs []apicontract.RunRecord) apicontract.HandlerFunc[apicontract.RunListArgs, apicontract.RunPage] {
	return func(_ context.Context, _ apicontract.Caller, q apicontract.RunListArgs) (apicontract.Call[apicontract.RunPage], error) {
		return func(context.Context) (apicontract.RunPage, error) {
			page := apicontract.RunPage{Runs: []apicontract.RunRecord{}}
			for _, run := range runs {
				upstream, _, _ := strings.Cut(run.Repo, "/")
				if len(q.Upstreams) == 0 || slices.Contains(q.Upstreams, upstream) {
					page.Runs = append(page.Runs, run)
				}
			}
			return page, nil
		}, nil
	}
}

// exampleEventsEverySecond answers like a busy server: the running run emits
// one event a second; a wait returns at once when events are pending, else at
// the next event or at its timeout.
func exampleEventsEverySecond(runID string) apicontract.HandlerFunc[apicontract.EventsWaitArgs, apicontract.EventResult] {
	var ticks atomic.Int64
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for range ticker.C {
			ticks.Add(1)
		}
	}()
	page := func(after, latest int64) model.EventPage {
		page := model.EventPage{NextAfterID: latest, LatestID: latest}
		for id := after + 1; id <= latest; id++ {
			page.Events = append(page.Events, model.AgentEvent{ID: id, Kind: "run.step", At: testNow(), RunID: runID, Summary: "step changed"})
		}
		return page
	}
	return func(_ context.Context, _ apicontract.Caller, q apicontract.EventsWaitArgs) (apicontract.Call[apicontract.EventResult], error) {
		return func(ctx context.Context) (apicontract.EventResult, error) {
			after := ticks.Load()
			if q.AfterID != nil {
				after = *q.AfterID
			}
			timeout := time.After(time.Duration(max(q.TimeoutS, 1)) * time.Second)
			for ticks.Load() <= after {
				select {
				case <-ctx.Done():
					return apicontract.EventResult{TimedOut: true}, ctx.Err()
				case <-timeout:
					return apicontract.EventResult{EventPage: page(after, after), TimedOut: true}, nil
				case <-time.After(50 * time.Millisecond):
				}
			}
			return apicontract.EventResult{EventPage: page(after, ticks.Load())}, nil
		}, nil
	}
}

// exampleImportedIssue serves #42-#46 as imported and native examples and the
// other listed issues with a short body.
func exampleImportedIssue(issue model.Issue, listed []model.Issue) apicontract.HandlerFunc[apicontract.IssueGetArgs, model.Issue] {
	return func(_ context.Context, _ apicontract.Caller, q apicontract.IssueGetArgs) (apicontract.Call[model.Issue], error) {
		v := issue.Clone()
		if (q.ID < 42 || q.ID > 46) && q.ID >= 1 && q.ID <= int64(len(listed)) {
			v = listed[q.ID-1].Clone()
			v.Body = "Seen on the default branch.\nThe step log names the failing command."
		}
		if q.ID == 45 || q.ID == 46 {
			v.ID, v.Title, v.Body = q.ID, "Native issue", "null"
			v.Occurrences = q.ID - 44
			v.Imported, v.History, v.SourceClosedAt = nil, nil, nil
			v.Author, v.Closer = "", ""
		}
		if q.ID == 43 || q.ID == 44 {
			v.ID = q.ID
			v.Closer = ""
		}
		if q.ID == 43 {
			v.SourceClosedAt = nil
		}
		return func(context.Context) (model.Issue, error) { return v, nil }, nil
	}
}
