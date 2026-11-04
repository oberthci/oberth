package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
)

func TestPassedRunTrailerMessages(t *testing.T) {
	sha := strings.Repeat("a", 40)
	for _, tc := range []struct {
		name           string
		trailers       []model.CommitIssueTrailer
		schedule, want string
	}{
		{name: "none"},
		{name: "close", trailers: []model.CommitIssueTrailer{{IssueID: 1347, SHA: sha, Close: true}}, want: "closes #1347"},
		{name: "ref", trailers: []model.CommitIssueTrailer{{IssueID: 1350, SHA: sha}}, want: "refs #1350"},
		{name: "close wins, sorted", trailers: []model.CommitIssueTrailer{{IssueID: 1416, SHA: sha, Close: true}, {IssueID: 1415, SHA: sha}, {IssueID: 1415, SHA: sha, Close: true}}, schedule: "nightly", want: "closes #1415, #1416 · schedule nightly"},
		{name: "mixed", trailers: []model.CommitIssueTrailer{{IssueID: 3, SHA: sha}, {IssueID: 2, SHA: sha, Close: true}}, want: "closes #2 · refs #3"},
		{name: "wrong commit", trailers: []model.CommitIssueTrailer{{IssueID: 1, SHA: strings.Repeat("b", 40), Close: true}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := model.Run{Status: model.RunPassed, SHA: sha, IssueTrailers: tc.trailers}
			if tc.schedule != "" {
				run.Trigger = "schedule"
				run.ScheduleEntry = tc.schedule
			}
			if got := presentationRun(run).DisplayReason; got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

type runTrailerGit struct {
	*controlGit
	reads      int
	input, sha string
	failure    error
}

func (g *runTrailerGit) CommitIssueTrailers(_ context.Context, input, sha string) ([]model.CommitIssueTrailer, error) {
	g.reads++
	g.input, g.sha = input, sha
	return []model.CommitIssueTrailer{{IssueID: 1478, SHA: sha, Close: true}}, g.failure
}

func TestSchedulerRecordsCommitTrailersForRunList(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "record", true: "optional read failure"}[fail], func(t *testing.T) {
			f := newControlFixture(t, JobResult{Status: model.RunPassed, Phase: "passed", Steps: []model.StepResult{stepResult(model.StepPassed)}})
			git := &runTrailerGit{controlGit: f.git}
			if fail {
				git.failure = errors.New("display unavailable")
			}
			f.scheduler.git = git
			run, err := f.scheduler.EnqueueCI(t.Context(), CIRequest{EventID: "run-trailers", Repository: f.repo, Branch: "feature/trailers", SHA: strings.Repeat("a", 40), Actor: "owner"})
			if err != nil {
				t.Fatal(err)
			}
			if err := f.scheduler.ProcessNext(t.Context()); err != nil {
				t.Fatal(err)
			}
			stored, err := f.store.Run(t.Context(), run.ID)
			if err != nil || stored.Status != model.RunPassed || git.reads != 1 || git.sha != run.SHA || git.input != "codeberg/acme/oberth" {
				t.Fatalf("claim metadata: %+v, %+v %v", stored, git, err)
			}
			want := "closes #1478"
			if fail {
				want = ""
			}
			service := f.api(t)
			value, err := service.Runs(t.Context(), api.Actor{Identity: "owner"}, api.RunFilter{})
			if err != nil {
				t.Fatal(err)
			}
			rows := value.([]model.Run)
			if len(rows) != 1 || rows[0].DisplayReason != want {
				t.Fatalf("passed list: %+v", rows)
			}
			if !fail {
				if err := f.scheduler.recordRunIssueTrailers(t.Context(), stored, git.input, f.scheduler.requireMutation); err != nil || git.reads != 1 {
					t.Fatal("receipt was read twice", err)
				}
			}
		})
	}
}
