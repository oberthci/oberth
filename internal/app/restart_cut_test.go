package app

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runprogress"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/pkg/periapsis"
)

type restartReadController struct {
	adoptingArgoControl
	observed         argojob.Completion
	next             argojob.Completion
	observationError error
	readError        error
	resumedReads     int
	readEvents       []runprogress.Event
}

func (c *restartReadController) WaitResumed(_ context.Context, name, runID string, _ io.Writer, _ []runprogress.Event, _ int64, binding argojob.Adoption) (argojob.Completion, error) {
	if name != c.request.Name || runID != c.request.RunID || binding != c.binding {
		panic("adoption identity lost")
	}
	return c.observed, c.observationError
}
func (c *restartReadController) TerminalStateResumed(_ context.Context, name, runID string, binding argojob.Adoption, events []runprogress.Event) (*argojob.Completion, error) {
	if name != c.request.Name || runID != c.request.RunID || binding != c.binding {
		panic("fallback adoption identity lost")
	}
	c.resumedReads++
	c.readEvents = events
	return &c.next, c.readError
}

func TestRestartCutArgoJobsMapsInterruptionAndKeepsFallbackBinding(t *testing.T) {
	cut := argojob.Completion{Name: "workflow", Interrupted: true, Infrastructure: true, Reason: "server restart interrupted build/test: missing exit", Steps: []argojob.StepResult{{Burn: "build", Step: "test", Status: runprogress.StepFailed, ExitCode: 1}}}
	real := argojob.Completion{Name: "workflow", FailedBurn: "build", FailedStep: "test", Reason: "test exited 2", Steps: []argojob.StepResult{{Burn: "build", Step: "test", Status: runprogress.StepFailed, ExitCode: 2, ExitCodeKnown: true}}}
	for _, mode := range []string{"normal", "terminal-log-error", "late-terminal", "late-real-failure", "real-failure", "earlier-real-failure", "read-error"} {
		t.Run(mode, func(t *testing.T) {
			c := &restartReadController{adoptingArgoControl: adoptingArgoControl{found: true, binding: argojob.Adoption{UID: "workflow-uid", Identity: "identity", Size: periapsis.M}}, observed: cut, next: cut}
			if mode != "normal" {
				c.observationError = errors.New("log stream failed")
			}
			switch mode {
			case "late-terminal":
				c.observed = argojob.Completion{}
			case "late-real-failure":
				c.next = real
			case "earlier-real-failure":
				c.observed = real
			case "real-failure":
				c.observed, c.next = real, real
			case "read-error":
				c.readError = errors.New("foreign binding")
			}
			jobs := &ArgoJobs{controller: c, intents: map[string]argoIntent{}}
			run := model.Run{ID: "run", JobName: "workflow", RefKind: model.RefBranch, Ref: "feature", SHA: "sha", Trigger: "branch"}
			if _, found, err := jobs.Adopt(t.Context(), run); err != nil || !found {
				t.Fatal(found, err)
			}
			result, err := jobs.WaitResumed(t.Context(), run.JobName, io.Discard, nil, 0)
			if mode == "normal" {
				if err != nil || result.Status != model.RunInterrupted || result.Phase != "interrupted" || result.FailedStep != "" || len(jobs.intents) != 0 {
					t.Fatal("interruption mapping failed", result, err)
				}
				return
			}
			if !errors.Is(err, c.observationError) || len(jobs.intents) != 0 || result.Reobserve == nil {
				t.Fatal("error discarded bound re-observation capability", result, err, jobs.intents)
			}
			result, err = result.Reobserve(t.Context())
			if c.resumedReads != 1 || len(jobs.intents) != 0 {
				t.Fatal("fallback bypassed or leaked binding", c.resumedReads, jobs.intents)
			}
			if mode == "read-error" {
				if !errors.Is(err, c.readError) {
					t.Fatal("binding error became an interruption", result, err)
				}
				return
			}
			want := model.RunInterrupted
			if mode == "late-real-failure" || mode == "real-failure" || mode == "earlier-real-failure" {
				want = model.RunFailed
			}
			if err != nil || result.Status != want {
				t.Fatal("fresh terminal verdict replaced incorrectly", result, err)
			}
			if want == model.RunInterrupted && (result.FailedStep != "" || result.Phase != "interrupted") {
				t.Fatal("interruption kept test attribution", result)
			}
			if want == model.RunFailed && (result.FailedStep != "test" || result.Phase != "build") {
				t.Fatal("real failure lost test attribution", result)
			}
		})
	}
}

var _ service.JobAdopter = (*ArgoJobs)(nil)

func TestRestartCutFallbackOwnsJournalAndNewlyObservedExit(t *testing.T) {
	start := time.Now().UTC()
	finish := start.Add(time.Second)
	completion := argojob.Completion{Name: "workflow", FailedBurn: "build", FailedStep: "test", Steps: []argojob.StepResult{{Burn: "build", Step: "test", Status: runprogress.StepFailed, ExitCode: 2, ExitCodeKnown: true, StartedAt: &start, FinishedAt: &finish}}}
	c := &restartReadController{adoptingArgoControl: adoptingArgoControl{found: true, binding: argojob.Adoption{UID: "workflow-uid", Identity: "identity", Size: periapsis.M}}, observed: completion, next: completion}
	jobs := &ArgoJobs{controller: c, intents: map[string]argoIntent{}}
	run := model.Run{ID: "run", JobName: "workflow", RefKind: model.RefBranch, Ref: "feature", SHA: "sha", Trigger: "branch"}
	if _, found, err := jobs.Adopt(t.Context(), run); err != nil || !found {
		t.Fatal(found, err)
	}
	retained := []runprogress.Event{{Version: runprogress.Version, Burn: "build", Step: "test", Ordinal: 0, Status: runprogress.StepRunning, StartedAt: &start}}
	savedStart, savedFinish := start, finish
	result, err := jobs.WaitResumed(t.Context(), run.JobName, io.Discard, retained, 0)
	if err != nil || result.Reobserve == nil || len(jobs.intents) != 0 {
		t.Fatal("successful Wait lost fallback binding", result, err)
	}
	retained[0].Burn = "foreign"
	start = start.Add(time.Hour)
	finish = finish.Add(time.Hour)
	if _, err := result.Reobserve(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(c.readEvents) != 2 || c.readEvents[0].Burn != "build" || !c.readEvents[0].StartedAt.Equal(savedStart) || c.readEvents[1].ExitCode != 2 || !c.readEvents[1].StartedAt.Equal(savedStart) || !c.readEvents[1].FinishedAt.Equal(savedFinish) {
		t.Fatal("fallback evidence aliased or discarded", c.readEvents)
	}
}
