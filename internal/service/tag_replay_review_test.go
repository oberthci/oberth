package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

func observedTestFailure() JobResult {
	return observedTagResult(JobResult{Status: model.RunFailed, Phase: "test", FailedBurn: "test", FailedStep: "unit", Steps: []model.StepResult{stepResult(model.StepFailed)}})
}
func requireTagForce(t *testing.T, h *rerunHarness, id string) {
	t.Helper()
	if _, err := h.call(rerunAdmin, "run_rerun", map[string]any{"id": id}); !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), "force") {
		t.Fatalf("unsafe rerun was not force-gated: %v", err)
	}
	if _, err := h.call(rerunAdmin, "run_retry", map[string]any{"id": id}); err == nil {
		t.Fatal("unsafe tag retry accepted")
	}
}

func TestTagReplayReviewWIFStartedRequiresForce(t *testing.T) {
	result := observedTagResult(infraFailure)
	result.TerminalObservation.Nodes = 1
	result.TerminalObservation.Steps = []model.RunObservedStep{{Burn: "publish", Step: "push", Status: model.StepFailed, Started: true}}
	h := newRerunHarness(t, result)
	plan := tagReplayPlan()
	plan[1].Secrets = false
	plan[1].Credentialed = true
	h.fixture.jobs.plan = plan
	source := admitRerunTag(t, h)
	ensureTagJournal(t, h, source)
	requireTagForce(t, h, source.ID)
	if got := h.rerun(t, rerunAdmin, map[string]any{"id": source.ID, "force": true}); got.Warning == "" {
		t.Fatal("WIF forced replay lost warning")
	}
}

func TestTagReplayReviewObservationGapNeedsForce(t *testing.T) {
	for _, kind := range []string{"infrastructure", "job", "interrupted", "wait outage"} {
		t.Run(kind, func(t *testing.T) {
			result := JobResult{Status: model.RunFailed, Phase: kind}
			if kind == "interrupted" {
				result.Status = model.RunInterrupted
			}
			h := newRerunHarness(t, result)
			h.fixture.jobs.plan = tagReplayPlan()
			if kind == "wait outage" {
				h.fixture.jobs.waitErr = errors.New("Workflow reads unavailable")
			}
			source := admitRerunTag(t, h)
			ensureTagJournal(t, h, source)
			if _, err := h.fixture.store.LatestAuditActionByType(t.Context(), "run.observed-terminal", "run"); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("synthesized result gained terminal marker: %v", err)
			}
			requireTagForce(t, h, source.ID)
		})
	}
}

type deferredReplayCancel struct{ *retryEnqueueObserver }

func (*deferredReplayCancel) AcceptCancellations(context.Context, []model.RunCancellation) error {
	return errors.New("deletion unavailable")
}
func TestTagReplayReviewPendingDeletionRefusesEvenForce(t *testing.T) {
	h := newRerunHarness(t)
	source, err := h.fixture.store.EnqueueRun(t.Context(), model.RunSpec{RepoID: h.fixture.repo.ID, RefKind: model.RefTag, Ref: rerunTag, SHA: rerunTagObject, TestedSHA: rerunTagCommit, Actor: "pusher", Release: true, Credentialed: true, Trigger: "tag"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.fixture.store.ClaimNextRun(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.fixture.store.SetRunJobName(t.Context(), source.ID, "live-source"); err != nil {
		t.Fatal(err)
	}
	h.service.runCancels = h.fixture.store
	h.service.enqueues = &deferredReplayCancel{&retryEnqueueObserver{}}
	value, err := h.call(rerunAdmin, "run_cancel", map[string]any{"id": source.ID, "force": true})
	if err != nil {
		t.Fatal(err)
	}
	if value.(api.RunCancelResponse).Workflow != cancelWorkflowPending {
		t.Fatalf("cancel was not deferred: %+v", value)
	}
	for _, tool := range []string{"run_retry", "run_rerun"} {
		for _, force := range []bool{false, true} {
			args := map[string]any{"id": source.ID}
			if tool == "run_rerun" {
				args["force"] = force
			}
			if _, err := h.call(rerunAdmin, tool, args); !errors.Is(err, store.ErrInvalidState) || !strings.Contains(err.Error(), "deletion") {
				t.Fatalf("pending deletion bypassed by %s force=%v: %v", tool, force, err)
			}
		}
	}
	counts, err := h.fixture.store.CountRuns(t.Context())
	if err != nil || counts.Total != 1 {
		t.Fatalf("pending deletion queued work: %+v %v", counts, err)
	}
	if err := h.fixture.store.CompleteRunCancellation(t.Context(), source.ID, "live-source"); err != nil {
		t.Fatal(err)
	}
	h.rerun(t, rerunAdmin, map[string]any{"id": source.ID, "force": true})
}

func TestTagReplayReviewWholeChainAndNewest(t *testing.T) {
	for _, unsafe := range []bool{false, true} {
		t.Run(map[bool]string{false: "older-safe-source", true: "newest-safe-source-unsafe-history"}[unsafe], func(t *testing.T) {
			first := observedTestFailure()
			if unsafe {
				first = observedTagResult(infraFailure)
				first.TerminalObservation.Nodes = 1
				first.TerminalObservation.Steps = []model.RunObservedStep{{Burn: "publish", Step: "push", Status: model.StepFailed, Started: true}}
			}
			second := observedTestFailure()
			if unsafe {
				second = observedTagResult(JobResult{Status: model.RunFailed, Phase: "job", Steps: []model.StepResult{stepResult(model.StepPassed)}})
			}
			h := newRerunHarness(t, first, second)
			h.fixture.jobs.plan = tagReplayPlan()
			a := admitRerunTag(t, h)
			ensureTagJournal(t, h, a)
			b := h.rerun(t, rerunAdmin, map[string]any{"id": a.ID, "force": unsafe})
			h.process(t)
			current := h.run(t, b.RunID)
			ensureTagJournal(t, h, current)
			// Older A needs force even if both attempts are safe.
			requireTagForce(t, h, a.ID)
			if unsafe {
				requireTagForce(t, h, current.ID)
			} else {
				h.rerun(t, rerunAdmin, map[string]any{"id": current.ID})
				audit, err := h.fixture.store.LatestAuditActionByType(t.Context(), "run.rerun", "run")
				if err != nil || !strings.Contains(audit.Details, `"replay_runs_checked":2`) || !strings.Contains(audit.Details, `"replay_steps_checked":4`) {
					t.Fatalf("whole-chain audit incomplete: %+v %v", audit, err)
				}
			}
		})
	}
}

func TestTagReplayReviewSafeAncestorCannotHideNewPublisher(t *testing.T) {
	publisher := observedTagResult(infraFailure)
	publisher.TerminalObservation.Nodes = 1
	publisher.TerminalObservation.Steps = []model.RunObservedStep{{Burn: "publish", Step: "push", Status: model.StepFailed, Started: true}}
	h := newRerunHarness(t, observedTestFailure(), publisher)
	h.fixture.jobs.plan = tagReplayPlan()
	a := admitRerunTag(t, h)
	ensureTagJournal(t, h, a)
	b := h.rerun(t, rerunAdmin, map[string]any{"id": a.ID})
	h.process(t)
	ensureTagJournal(t, h, h.run(t, b.RunID))
	requireTagForce(t, h, a.ID)
	requireTagForce(t, h, b.RunID)
}

func TestTagReplayReviewMovedOrMissingTagRequiresForce(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "moved", true: "deleted"}[missing], func(t *testing.T) {
			h := newRerunHarness(t, observedTestFailure())
			h.fixture.jobs.plan = tagReplayPlan()
			source := admitRerunTag(t, h)
			ensureTagJournal(t, h, source)
			refs := h.refs.branches["codeberg/acme/oberth"]
			if missing {
				delete(refs, "refs/tags/"+rerunTag)
			} else {
				refs["refs/tags/"+rerunTag] = strings.Repeat("d", 40)
			}
			requireTagForce(t, h, source.ID)
			h.rerun(t, rerunAdmin, map[string]any{"id": source.ID, "force": true})
		})
	}
}

func TestTagReplayReviewLegacyPlanAndMissingJournalRequireForce(t *testing.T) {
	for _, kind := range []string{"legacy-plan", "missing-journal"} {
		t.Run(kind, func(t *testing.T) {
			h := newRerunHarness(t, observedTestFailure())
			h.fixture.jobs.plan = tagReplayPlan()
			source := admitRerunTag(t, h)
			ensureTagJournal(t, h, source)
			if kind == "legacy-plan" {
				body := `{"version":1,"steps":[{"burn":"test","step":"unit","ordinal":0},{"burn":"publish","step":"push","ordinal":1}]}`
				if err := os.WriteFile(filepath.Join(h.fixture.root, "logs", source.ID+".plan.json"), []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Remove(filepath.Join(h.fixture.root, "logs", source.ID+".progress.jsonl")); err != nil {
				t.Fatal(err)
			}
			requireTagForce(t, h, source.ID)
		})
	}
}

func TestTagReplayReviewAuditBasis(t *testing.T) {
	h := newRerunHarness(t, observedTestFailure())
	h.fixture.jobs.plan = tagReplayPlan()
	source := admitRerunTag(t, h)
	ensureTagJournal(t, h, source)
	h.rerun(t, rerunAdmin, map[string]any{"id": source.ID})
	audit, err := h.fixture.store.LatestAuditActionByType(t.Context(), "run.rerun", "run")
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{`"release":true`, `"replay_basis":"observed-terminal-no-credential-start"`, `"replay_runs_checked":1`, `"replay_steps_checked":2`, `"publication_started":false`} {
		if !strings.Contains(audit.Details, part) {
			t.Fatalf("audit missing %s: %s", part, audit.Details)
		}
	}
}

func TestTagReplayReviewHistoryLimitAndUnknownResolverFailClosed(t *testing.T) {
	for _, kind := range []string{"history-limit", "unknown-resolver"} {
		t.Run(kind, func(t *testing.T) {
			h := newRerunHarness(t, observedTestFailure())
			h.fixture.jobs.plan = tagReplayPlan()
			source := admitRerunTag(t, h)
			ensureTagJournal(t, h, source)
			if kind == "unknown-resolver" {
				h.service.refs = struct{ RefResolver }{h.refs}
			} else {
				for range 64 {
					queued, err := h.fixture.store.EnqueueRun(t.Context(), model.RunSpec{RepoID: h.fixture.repo.ID, RefKind: model.RefTag, Ref: rerunTag, SHA: rerunTagObject, TestedSHA: rerunTagCommit, Actor: "pusher", Release: true, Credentialed: true, Trigger: "tag"})
					if err != nil {
						t.Fatal(err)
					}
					source, err = h.fixture.store.FinishRun(t.Context(), queued.ID, model.RunResult{Status: model.RunFailed, Phase: "infrastructure"})
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			requireTagForce(t, h, source.ID)
		})
	}
}
