package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
)

type changingPromotionPage struct {
	PromotionRepository
	afterList func()
}

func (s *changingPromotionPage) ListPromotions(ctx context.Context, f model.PromotionListFilter) (model.PromotionPage, error) {
	page, err := s.PromotionRepository.ListPromotions(ctx, f)
	if err == nil && s.afterList != nil {
		fn := s.afterList
		s.afterList = nil
		fn()
	}
	return page, err
}

func TestPromotionListRefreshesPendingAtFinalizationBoundary(t *testing.T) {
	for _, filter := range []model.PromotionStatus{"", model.PromotionPending} {
		t.Run(string(filter), func(t *testing.T) {
			f := newControlFixture(t)
			promotion, pub := beginPendingPromotionPublication(t, f, "main", retryBaseSHA, retrySourceSHA)
			control := f.api(t)
			control.promotions = &changingPromotionPage{PromotionRepository: f.store, afterList: func() {
				if _, err := f.store.FinalizePublication(t.Context(), pub.ID, model.PublicationDelivered, ""); err != nil {
					t.Fatal(err)
				}
			}}
			page, err := control.promotionList(t.Context(), promotionListArguments{Status: filter})
			if err != nil {
				t.Fatal(err)
			}
			status := promoteStatus(t, control, api.Actor{Identity: "agent@host"}, promotion.ID)
			if status.Promotion.Status != model.PromotionPassed {
				t.Fatal(status)
			}
			if filter == model.PromotionPending {
				if len(page.Promotions) != 0 {
					t.Fatalf("terminal promotion still listed as pending: %+v", page)
				}
			} else if len(page.Promotions) != 1 || page.Promotions[0].Status != string(status.Promotion.Status) {
				t.Fatalf("list/status disagree: %+v %+v", page, status)
			}
		})
	}
}

type promotionFinalizesOnRead struct {
	PromotionRepository
	reads    int
	finalize func()
}

func (s *promotionFinalizesOnRead) Promotion(ctx context.Context, id string) (model.Promotion, error) {
	p, err := s.PromotionRepository.Promotion(ctx, id)
	s.reads++
	if err == nil && s.reads == 2 {
		s.finalize()
	}
	return p, err
}
func TestPromotionTimeoutRechecksDurableCompletion(t *testing.T) {
	f := newControlFixture(t)
	promotion, pub := beginPendingPromotionPublication(t, f, "main", retryBaseSHA, retrySourceSHA)
	control := f.api(t)
	control.maximumWait = time.Nanosecond
	control.promotions = &promotionFinalizesOnRead{PromotionRepository: f.store, finalize: func() {
		if _, err := f.store.FinalizePublication(t.Context(), pub.ID, model.PublicationDelivered, ""); err != nil {
			t.Fatal(err)
		}
	}}
	prepared, err := control.prepareWaitPromotion(t.Context(), promotion.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	status, err := prepared(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	page, err := control.promotionList(t.Context(), promotionListArguments{})
	if err != nil {
		t.Fatal(err)
	}
	if status.StillRunning || status.Promotion.Status != model.PromotionPassed || page.Promotions[0].Status != string(status.Promotion.Status) {
		t.Fatalf("timeout returned stale pending: %+v %+v", status, page)
	}
}

func TestPromotionViewsUseLedgerUntilRecovery(t *testing.T) {
	f := newControlFixture(t)
	promotion, pub := beginPendingPromotionPublication(t, f, "main", retryBaseSHA, retrySourceSHA)
	f.git.setRemoteRefLocked("refs/heads/main", retrySourceSHA)
	control := f.api(t)
	for _, want := range []model.PromotionStatus{model.PromotionPending, model.PromotionPassed} {
		if want == model.PromotionPassed {
			if err := f.scheduler.recoverPendingPublications(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		before, err := f.store.VerifyAuditChain(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		page, err := control.promotionList(t.Context(), promotionListArguments{Status: want})
		if err != nil {
			t.Fatal(err)
		}
		status := promoteStatus(t, control, api.Actor{Identity: "agent@host"}, promotion.ID)
		after, err := f.store.VerifyAuditChain(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if status.Promotion.Status != want || len(page.Promotions) != 1 || page.Promotions[0].Status != string(want) || before.ID != after.ID {
			t.Fatalf("views invented completion or mutated state: %+v %+v", page, status)
		}
	}
	if len(f.git.promotions) != 0 {
		t.Fatal("read-back repeated the push", pub.ID)
	}
}

func failedPublicationForStatus(t *testing.T) (*controlFixture, model.Run, model.Publication) {
	t.Helper()
	f := newControlFixture(t)
	queued, err := f.scheduler.EnqueueCI(t.Context(), CIRequest{EventID: "status-failure", Repository: f.repo, Branch: "feature/status", SHA: retrySourceSHA, Actor: "agent@host"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := f.store.ClaimNextRun(t.Context())
	if err != nil || run.ID != queued.ID {
		t.Fatal(err)
	}
	pub, err := f.store.BeginPublication(t.Context(), model.PublicationSpec{RepoID: f.repo.ID, RunID: run.ID, RefKind: run.RefKind, Ref: run.Ref, ResultSHA: run.SHA, Actor: run.Actor})
	if err != nil {
		t.Fatal(err)
	}
	final, err := f.store.FinalizePublication(t.Context(), pub.ID, model.PublicationFailed, "permission denied by upstream")
	if err != nil {
		t.Fatal(err)
	}
	return f, final.Run, final.Publication
}
func TestFailedRunListNamesPublicationCause(t *testing.T) {
	f, run, _ := failedPublicationForStatus(t)
	control := f.api(t)
	actor := api.Actor{Identity: "agent@host"}
	listed, err := control.Runs(t.Context(), actor, api.RunFilter{})
	if err != nil {
		t.Fatal(err)
	}
	detail, err := control.Run(t.Context(), actor, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	row := listed.([]model.Run)[0]
	shown := detail.(RunDetailResponse).Run
	if row.DisplayReason == "" || !strings.Contains(row.DisplayReason, "upstream") || row.DisplayReason != shown.DisplayReason {
		t.Fatalf("failed list hides publication cause: %+v %+v", row, shown)
	}
	for _, phase := range []string{"", "job", "checkout", "infrastructure"} {
		if got := presentationRun(model.Run{Status: model.RunFailed, Phase: phase}).DisplayReason; got == "" {
			t.Fatalf("failed %q run lacks reason", phase)
		}
	}
}

type statusDeliveryObserver struct {
	EnqueueObserver
	deliver func(context.Context, string) (model.PublicationFinalization, error)
}

func (s statusDeliveryObserver) DeliverPublication(ctx context.Context, id string) (model.PublicationFinalization, error) {
	return s.deliver(ctx, id)
}
func outcomeJSON(t *testing.T, value any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err = json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	return fields
}

func TestRetryResponsesDescribeActionAndDurableRun(t *testing.T) {
	for _, tool := range []string{"publish_retry", "run_rerun"} {
		for _, outcome := range []string{"queued", "delivered", "failed", "stale delivery reply"} {
			t.Run(tool+"/"+outcome, func(t *testing.T) {
				f, run, _ := failedPublicationForStatus(t)
				control := f.api(t)
				control.runReruns = f.store
				if outcome == "queued" {
					f.scheduler.git = &retryPublicationGit{controlGit: f.git, failure: errors.New("remote: Internal Server Error")}
				}
				if outcome == "delivered" {
					control.enqueues = lostPublicationAck{control.enqueues}
				}
				if outcome == "failed" || outcome == "stale delivery reply" {
					control.enqueues = statusDeliveryObserver{EnqueueObserver: control.enqueues, deliver: func(ctx context.Context, id string) (model.PublicationFinalization, error) {
						final, err := f.store.FinalizePublication(ctx, id, model.PublicationFailed, "permission denied")
						if err != nil {
							return final, err
						}
						if outcome == "stale delivery reply" {
							final.Publication.Status = model.PublicationPending
						}
						return final, nil
					}}
				}
				actor := api.Actor{Identity: "admin@host", Admin: true}
				value, err := control.CallTool(t.Context(), actor, tool, json.RawMessage(`{"id":"`+run.ID+`"}`))
				if err != nil {
					t.Fatal(err)
				}
				response := outcomeJSON(t, value)
				detail, err := control.Run(t.Context(), actor, run.ID)
				if err != nil {
					t.Fatal(err)
				}
				shown := detail.(RunDetailResponse).Run
				if response["action"] != "publication_retry" || response["message"] == "" || response["run_status"] != string(shown.Status) || response["run_phase"] != shown.Phase {
					t.Fatalf("retry response disagrees with run_get: %+v %+v", response, shown)
				}
				if outcome == "stale delivery reply" && response["status"] != "failed" {
					t.Fatal("trusted stale delivery return instead of durable outcome", response)
				}
				if outcome == "queued" {
					if !strings.Contains(shown.Error, "retry 2/5") || shown.DisplayReason != "publishing to upstream" || shown.DisplayStatus != "running" {
						t.Fatalf("publication progress hidden by queue priority: %+v", shown)
					}
				}
				if len(f.jobs.createdCI) != 0 {
					t.Fatal("publication-only retry executed CI")
				}
			})
		}
	}
}
func TestRerunResponseNamesFullRunAndRefusalStartsNothing(t *testing.T) {
	h := newRerunHarness(t, flakyResult)
	enqueued, err := h.scheduler.EnqueueCI(t.Context(), CIRequest{EventID: "status-rerun", Repository: h.fixture.repo, Branch: rerunBranch, SHA: rerunSHA, Actor: "agent@host"})
	if err != nil {
		t.Fatal(err)
	}
	h.process(t)
	value, err := h.call(rerunAdmin, "run_rerun", map[string]any{"id": enqueued.ID})
	if err != nil {
		t.Fatal(err)
	}
	response := outcomeJSON(t, value)
	detail, err := h.service.Run(t.Context(), rerunAdmin, response["run_id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if response["action"] != "rerun" || response["message"] == "" || response["run_status"] != string(detail.(RunDetailResponse).Run.Status) {
		t.Fatalf("full rerun response = %+v", response)
	}
	for _, tool := range []string{"run_rerun", "publish_retry"} {
		_, err := h.call(rerunAdmin, tool, map[string]any{"id": ""})
		if err == nil || !strings.Contains(err.Error(), "nothing started") {
			t.Fatalf("refusal hid that no work started: %s %v", tool, err)
		}
	}
}

type unavailablePublicationState struct{ PromotionRepository }

func (s unavailablePublicationState) PublicationState(context.Context, string) (model.PublicationFinalization, error) {
	return model.PublicationFinalization{}, errors.New("status reader unavailable")
}
func TestAcceptedRetryWithUnreadableStatusDoesNotInventPending(t *testing.T) {
	f, run, _ := failedPublicationForStatus(t)
	control := f.api(t)
	control.promotions = unavailablePublicationState{f.store}
	response, err := control.publishRetry(t.Context(), api.Actor{Identity: "agent@host"}, run.ID)
	if err != nil || response.Action != "publication_retry" || response.Status != "unknown" || response.RunStatus != "unknown" || !strings.Contains(response.Message, "started") {
		t.Fatalf("unreadable status was guessed: %+v %v", response, err)
	}
	current, err := f.store.Run(t.Context(), run.ID)
	if err != nil || current.Status != model.RunPassed {
		t.Fatalf("accepted delivery changed: %+v %v", current, err)
	}
}

type lostRerunDispatchAck struct{ EnqueueObserver }

func (s lostRerunDispatchAck) AcceptEnqueue(ctx context.Context, run model.EnqueueRunResult) error {
	if err := s.EnqueueObserver.AcceptEnqueue(ctx, run); err != nil {
		return err
	}
	return errors.New("dispatch acknowledgement lost")
}
func TestAdmittedRerunErrorDoesNotClaimNothingStarted(t *testing.T) {
	h := newRerunHarness(t, flakyResult)
	enqueued, err := h.scheduler.EnqueueCI(t.Context(), CIRequest{EventID: "status-rerun-ack", Repository: h.fixture.repo, Branch: rerunBranch, SHA: rerunSHA, Actor: "agent@host"})
	if err != nil {
		t.Fatal(err)
	}
	h.process(t)
	h.service.enqueues = lostRerunDispatchAck{h.service.enqueues}
	value, err := h.call(rerunAdmin, "run_rerun", map[string]any{"id": enqueued.ID})
	if err == nil || strings.Contains(err.Error(), "nothing started") || !strings.Contains(err.Error(), "Full rerun started") {
		t.Fatalf("accepted work described as refused: %v", err)
	}
	response := value.(api.RunRerunResponse)
	run, readErr := h.fixture.store.Run(t.Context(), response.RunID)
	if readErr != nil || run.Status != model.RunQueued || response.Action != "rerun" {
		t.Fatalf("admitted work lost: %+v %v", response, readErr)
	}
}

func TestFailedRunDisplayCoversTransientAndPermanentPublication(t *testing.T) {
	for _, tc := range []struct{ phase, message, want string }{
		{"publishing", "remote: Internal Server Error", "steps passed, upstream publication failed"},
		{"infrastructure", "upstream publication infrastructure failure: all 5 attempts exhausted", "steps passed, upstream publication failed"},
		{"publishing", "non-fast-forward", "upstream rejected: non-fast-forward"},
		{"publishing", "", "steps passed, upstream publication failed"},
		{"checkout", "object unavailable", "checkout error"},
	} {
		run := model.Run{Status: model.RunFailed, Phase: tc.phase, Error: tc.message}
		got := presentationRun(run)
		if !strings.Contains(got.DisplayReason, tc.want) || got.Error != tc.message || got.Phase != tc.phase || got.Status != run.Status {
			t.Fatalf("display changed durable cause or omitted phase: %+v", got)
		}
	}
}

type lostPublicationAdmissionAck struct{ PromotionRepository }

func (s lostPublicationAdmissionAck) RetryFailedPublication(ctx context.Context, runID, promotionID, actor string) (model.Publication, error) {
	pub, err := s.PromotionRepository.RetryFailedPublication(ctx, runID, promotionID, actor)
	if err != nil {
		return pub, err
	}
	return pub, context.DeadlineExceeded
}
func TestUnconfirmedPublicationAdmissionIsNotReportedAsNothingStarted(t *testing.T) {
	for _, tool := range []string{"publish_retry", "run_rerun"} {
		t.Run(tool, func(t *testing.T) {
			f, run, _ := failedPublicationForStatus(t)
			control := f.api(t)
			control.runReruns = f.store
			control.promotions = lostPublicationAdmissionAck{f.store}
			value, err := control.CallTool(t.Context(), api.Actor{Identity: "admin@host", Admin: true}, tool, json.RawMessage(`{"id":"`+run.ID+`"}`))
			if err == nil || strings.Contains(err.Error(), "nothing started") || !strings.Contains(err.Error(), "unconfirmed") {
				t.Fatalf("ambiguous admission misreported: %+v %v", value, err)
			}
			fields := outcomeJSON(t, value)
			if fields["action"] != "unknown" {
				t.Fatalf("lost admission metadata: %+v", fields)
			}
			current, readErr := f.store.Run(t.Context(), run.ID)
			if readErr != nil || current.Status != model.RunRunning || current.Phase != "publishing" {
				t.Fatalf("durable admission lost: %+v %v", current, readErr)
			}
		})
	}
}

func TestMCPRetryOutcomeSurvivesPrivateErrorRedaction(t *testing.T) {
	t.Run("nothing started", func(t *testing.T) {
		f := newControlFixture(t)
		control := f.api(t)
		control.mutationGate = func(context.Context) error { return errors.New("private-status-canary") }
		result := resultCall(t, resultServer(t, control, api.Actor{Identity: "admin@host", Admin: true}), "publish_retry", map[string]any{"id": "unused"})
		if !result.IsError || !strings.Contains(result.Content[0].Text, "nothing started") || strings.Contains(result.Content[0].Text, "private-status-canary") {
			t.Fatalf("MCP lost action or leaked cause: %+v", result)
		}
	})
	t.Run("full rerun admitted", func(t *testing.T) {
		h := newRerunHarness(t, flakyResult)
		enqueued, err := h.scheduler.EnqueueCI(t.Context(), CIRequest{EventID: "status-mcp-rerun", Repository: h.fixture.repo, Branch: rerunBranch, SHA: rerunSHA, Actor: "agent@host"})
		if err != nil {
			t.Fatal(err)
		}
		h.process(t)
		h.service.enqueues = lostRerunDispatchAck{h.service.enqueues}
		result := resultCall(t, resultServer(t, h.service, rerunAdmin), "run_rerun", map[string]any{"id": enqueued.ID})
		if !result.IsError || !strings.Contains(result.Content[0].Text, "Full rerun started") || strings.Contains(result.Content[0].Text, "nothing started") || strings.Contains(result.Content[0].Text, "acknowledgement lost") {
			t.Fatalf("MCP hid admitted work or leaked cause: %+v", result)
		}
	})
}
