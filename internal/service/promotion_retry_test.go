package service

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/model"
)

const (
	retrySourceSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	retryBaseSHA   = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	retryMovedSHA  = "dddddddddddddddddddddddddddddddddddddddd"
	retryMergedSHA = "cccccccccccccccccccccccccccccccccccccccc"
	retryMerged2   = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
)

// scriptedPromotionGit serves one scripted merge plan per PreparePromotion
// and one scripted outcome per PushPromotion, so a test can make the target
// move under the first delivery and the retry land on the new head.
type scriptedPromotionGit struct {
	*controlGit
	mu       sync.Mutex
	plans    []gitcache.MergeCandidate
	pushes   []scriptedPush
	prepared int
	pushed   []string
}

type scriptedPush struct {
	err    error
	moveTo string // the remote target moves here before the push is answered
}

func (git *scriptedPromotionGit) PreparePromotion(_ context.Context, _, _, target, _ string) (gitcache.MergeCandidate, error) {
	git.mu.Lock()
	defer git.mu.Unlock()
	if len(git.plans) == 0 {
		return gitcache.MergeCandidate{}, errors.New("unexpected promotion preparation")
	}
	plan := git.plans[0]
	git.plans = git.plans[1:]
	git.prepared++
	git.controlGit.mu.Lock()
	git.setRemoteRefLocked("refs/heads/"+target, plan.BaseSHA)
	git.controlGit.mu.Unlock()
	return plan, nil
}

func (git *scriptedPromotionGit) PushPromotion(_ context.Context, _, target, sha string) error {
	git.mu.Lock()
	defer git.mu.Unlock()
	git.pushed = append(git.pushed, target+":"+sha)
	push := scriptedPush{}
	if len(git.pushes) > 0 {
		push = git.pushes[0]
		git.pushes = git.pushes[1:]
	}
	git.controlGit.mu.Lock()
	defer git.controlGit.mu.Unlock()
	if push.moveTo != "" {
		git.setRemoteRefLocked("refs/heads/"+target, push.moveTo)
	}
	if push.err != nil {
		return push.err
	}
	git.setRemoteRefLocked("refs/heads/"+target, sha)
	return nil
}

// stubUplinkDirectory serves current uplinks by identity; tests change it to
// revoke or remove the uplink between admission and the automatic retry.
type stubUplinkDirectory struct {
	mu      sync.Mutex
	uplinks map[string][]model.Uplink
}

func (directory *stubUplinkDirectory) UplinksByIdentity(_ context.Context, identity string) ([]model.Uplink, error) {
	directory.mu.Lock()
	defer directory.mu.Unlock()
	return append([]model.Uplink(nil), directory.uplinks[identity]...), nil
}

func (directory *stubUplinkDirectory) set(identity string, uplinks ...model.Uplink) {
	directory.mu.Lock()
	defer directory.mu.Unlock()
	directory.uplinks[identity] = uplinks
}

func (git *scriptedPromotionGit) counts() (prepared, pushed int) {
	git.mu.Lock()
	defer git.mu.Unlock()
	return git.prepared, len(git.pushed)
}

// newPromotionRetryHarness wires a scheduler and an API over the scripted
// Git, with the API registered as the scheduler's moved-target retrier, the
// way serve wires them.
func newPromotionRetryHarness(t *testing.T, git *scriptedPromotionGit, results ...JobResult) (*controlFixture, *Scheduler, *API) {
	t.Helper()
	fixture, scheduler, service, _ := newPromotionRetryHarnessWithUplinks(t, git, results...)
	return fixture, scheduler, service
}

func newPromotionRetryHarnessWithUplinks(t *testing.T, git *scriptedPromotionGit, results ...JobResult) (*controlFixture, *Scheduler, *API, *stubUplinkDirectory) {
	t.Helper()
	fixture := newControlFixture(t, results...)
	uplinks := &stubUplinkDirectory{uplinks: map[string][]model.Uplink{
		"agent@host": {{Identity: "agent@host", Fingerprint: "SHA256:agent", Admin: true}},
		"admin@host": {{Identity: "admin@host", Fingerprint: "SHA256:admin", Admin: true}},
	}}
	git.controlGit = fixture.git
	scheduler, err := NewScheduler(SchedulerConfig{
		Store: fixture.store, Git: git, Logs: fixture.logs, Jobs: fixture.jobs, ReleaseJobs: fixture.jobs,
		Auditor: fixture.store, Signals: fixture.signals,
		WorkspaceRoot: filepath.Join(fixture.root, "retry-work"), MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewAPI(APIConfig{
		Runs: fixture.store, History: fixture.store, Repositories: fixture.store,
		Issues: fixture.store, Promotions: fixture.store, PromotionRuns: fixture.store,
		Enqueues: scheduler, Git: git, Logs: fixture.logs, Auditor: fixture.store,
		Signals: fixture.signals, MaximumWait: testTimeout(50 * time.Millisecond),
		PromotionWorkspaceRoot: filepath.Join(fixture.root, "retry-promotion-work"),
		PromotionGrants:        fixture.store, Uplinks: uplinks,
	})
	if err != nil {
		t.Fatal(err)
	}
	scheduler.SetPromotionRetrier(service)
	t.Cleanup(service.waitBackground)
	seedGreenPromotionCandidate(t, fixture, retrySourceSHA)
	return fixture, scheduler, service, uplinks
}

func promoteForRetry(t *testing.T, service *API, actor api.Actor) string {
	t.Helper()
	value, err := service.CallTool(context.Background(), actor, "promote", json.RawMessage(`{"sha":"`+retrySourceSHA+`","branch":"main"}`))
	if err != nil {
		t.Fatal(err)
	}
	response, ok := value.(api.PromoteResponse)
	if !ok || response.ID == "" {
		t.Fatalf("promote response = %#v", value)
	}
	return response.ID
}

func promoteStatus(t *testing.T, service *API, actor api.Actor, id string) PromotionWaitResponse {
	t.Helper()
	value, err := service.CallTool(context.Background(), actor, "promote_status", json.RawMessage(`{"id":"`+id+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	response, ok := value.(PromotionWaitResponse)
	if !ok {
		t.Fatalf("promote_status response = %#v", value)
	}
	return response
}

var passedPromotionCI = JobResult{Status: model.RunPassed, Phase: "passed", Steps: []model.StepResult{stepResult(model.StepPassed)}}

// A merged promotion whose delivery loses the race to a moved target is
// re-merged onto the new head, re-tested, and delivered, with no new commit
// and no caller action (#1097).
func TestMovedTargetPromotionIsRetriedOntoNewHeadAndDelivered(t *testing.T) {
	git := &scriptedPromotionGit{
		plans: []gitcache.MergeCandidate{
			{BaseSHA: retryBaseSHA, MergedSHA: retryMergedSHA},
			{BaseSHA: retryMovedSHA, MergedSHA: retryMerged2},
		},
		pushes: []scriptedPush{{err: errors.New("rejected: non-fast-forward"), moveTo: retryMovedSHA}, {}},
	}
	fixture, scheduler, service := newPromotionRetryHarness(t, git, passedPromotionCI, passedPromotionCI)
	admin := api.Actor{Identity: "agent@host", Admin: true}
	originalID := promoteForRetry(t, service, admin)

	// The merged CI passes, the push finds main moved, and the scheduler's
	// hook re-promotes onto the moved head, queueing re-test CI.
	if err := scheduler.ProcessNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	original, err := fixture.store.Promotion(context.Background(), originalID)
	if err != nil {
		t.Fatal(err)
	}
	if original.Status != model.PromotionFailed || !strings.Contains(original.Error, "found "+retryMovedSHA) {
		t.Fatalf("original promotion = %#v, want failed with main moved to %s", original, retryMovedSHA)
	}
	// Admission is synchronous, so the retry is visible at once; its merge
	// preparation runs in the background.
	status := promoteStatus(t, service, admin, originalID)
	service.waitBackground()
	if status.RetriedAs == "" || status.RetriedAs == originalID {
		t.Fatalf("promote_status of the moved promotion = %#v, want retried_as", status)
	}
	retry, err := fixture.store.Promotion(context.Background(), status.RetriedAs)
	if err != nil {
		t.Fatal(err)
	}
	if retry.Status != model.PromotionPending || retry.PreviousSHA != retryMovedSHA || retry.ResultSHA != retryMerged2 || retry.RunID == "" {
		t.Fatalf("retry before its CI = %#v, want pending re-test of a merge onto %s", retry, retryMovedSHA)
	}

	if err := scheduler.ProcessNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	retry, err = fixture.store.Promotion(context.Background(), retry.ID)
	if err != nil || retry.Status != model.PromotionPassed {
		t.Fatalf("retried promotion = %#v, %v, want passed", retry, err)
	}
	if git.pushed[len(git.pushed)-1] != "main:"+retryMerged2 {
		t.Fatalf("pushes = %v, want the re-tested merge delivered last", git.pushed)
	}
	lineage, err := fixture.store.PromotionLineage(context.Background(), retry.ID)
	if err != nil || lineage.RetryOf != originalID || lineage.Mode != model.PromotionRetryAuto || lineage.AutoAttempt != 1 {
		t.Fatalf("retry lineage = %#v, %v", lineage, err)
	}
	if action, err := fixture.store.LatestAuditActionByType(context.Background(), "promotion.retry", "promotion"); err != nil || action.ResourceID != retry.ID {
		t.Fatalf("promotion.retry audit = %#v, %v", action, err)
	}
	// The original stays failed: it is immutable history, not reactivated.
	if again, _ := fixture.store.Promotion(context.Background(), originalID); again.Status != model.PromotionFailed {
		t.Fatalf("original after retry delivered = %#v", again)
	}
}

// A target that keeps moving is chased at most maxAutoMovedPromotionRetries
// times; the last promotion stays failed for an explicit promote_retry.
func TestMovedTargetAutomaticRetryIsBounded(t *testing.T) {
	moved := []string{retryMovedSHA, retryMergedSHA, retryMerged2}
	git := &scriptedPromotionGit{}
	base := retryBaseSHA
	for _, next := range moved {
		git.plans = append(git.plans, gitcache.MergeCandidate{BaseSHA: base, MergedSHA: retrySourceSHA, FastForward: true})
		git.pushes = append(git.pushes, scriptedPush{err: errors.New("rejected"), moveTo: next})
		base = next
	}
	git.plans = append(git.plans, gitcache.MergeCandidate{BaseSHA: base, MergedSHA: retrySourceSHA, FastForward: true})
	fixture, _, service := newPromotionRetryHarness(t, git)
	admin := api.Actor{Identity: "agent@host", Admin: true}
	id := promoteForRetry(t, service, admin)
	service.waitBackground()

	chain := []string{id}
	for {
		status := promoteStatus(t, service, admin, chain[len(chain)-1])
		if status.Promotion.Status != model.PromotionFailed {
			t.Fatalf("promotion %d = %#v, want failed", len(chain), status.Promotion)
		}
		if status.RetriedAs == "" {
			break
		}
		chain = append(chain, status.RetriedAs)
	}
	if len(chain) != 1+maxAutoMovedPromotionRetries {
		t.Fatalf("automatic retries = %d, want %d", len(chain)-1, maxAutoMovedPromotionRetries)
	}
	if prepared, pushed := git.counts(); prepared != len(chain) || pushed != len(chain) {
		t.Fatalf("prepared %d and pushed %d times for %d promotions", prepared, pushed, len(chain))
	}
	last, err := fixture.store.PromotionLineage(context.Background(), chain[len(chain)-1])
	if err != nil || last.AutoAttempt != maxAutoMovedPromotionRetries || last.RetriedAs != "" {
		t.Fatalf("last lineage = %#v, %v", last, err)
	}
	// An explicit retry is still possible once the race is settled.
	value, err := service.CallTool(context.Background(), admin, "promote_retry", json.RawMessage(`{"id":"`+chain[len(chain)-1]+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	retried := value.(api.PromoteRetryResponse)
	if final, err := fixture.store.Promotion(context.Background(), retried.ID); err != nil || final.Status != model.PromotionPassed || retried.RetryOf != chain[len(chain)-1] {
		t.Fatalf("explicit retry = %#v (%#v), %v", final, retried, err)
	}
}

// A scoped uplink's grant survives a transient push failure: the promotion
// held it but nothing was delivered, so promote_retry with the same grant
// delivers, and only then is the grant consumed (#1097).
func TestTransientPushFailureDoesNotBurnPromotionGrant(t *testing.T) {
	git := &scriptedPromotionGit{
		plans: []gitcache.MergeCandidate{
			{BaseSHA: retryBaseSHA, MergedSHA: retrySourceSHA, FastForward: true},
			{BaseSHA: retryBaseSHA, MergedSHA: retrySourceSHA, FastForward: true},
		},
		// An SSH blip: the push fails and main does not move.
		pushes: []scriptedPush{{err: errors.New("ssh: connection reset by peer")}, {}},
	}
	fixture, _, service := newPromotionRetryHarness(t, git)
	ctx := context.Background()
	admin := api.Actor{Identity: "admin@host", Fingerprint: "SHA256:admin", Admin: true}
	uplink := api.Actor{Identity: "agent@host", Fingerprint: "SHA256:agent"}
	if _, err := service.CallTool(ctx, admin, "promote_grant",
		json.RawMessage(`{"repo":"oberth","sha":"`+retrySourceSHA+`","branch":"main","expires":"1h"}`)); err != nil {
		t.Fatal(err)
	}
	grant, err := fixture.store.MatchingPromotionGrant(ctx, fixture.repo.ID, retrySourceSHA, "main")
	if err != nil {
		t.Fatal(err)
	}

	originalID := promoteForRetry(t, service, uplink)
	original := promoteStatus(t, service, uplink, originalID)
	if original.Promotion.Status != model.PromotionFailed || original.RetriedAs != "" || original.Promotion.GrantID != grant.ID {
		t.Fatalf("transiently failed promotion = %#v, want failed, holding the grant, not auto-retried", original)
	}
	if held, err := fixture.store.PromotionGrant(ctx, grant.ID); err != nil || held.ConsumedAt != nil {
		t.Fatalf("grant after a failed delivery = %#v, %v, want unconsumed", held, err)
	}
	if reserve, err := fixture.store.LatestAuditActionByType(ctx, "promotion_grant.reserve", "promotion_grant"); err != nil || reserve.ResourceID != grant.ID {
		t.Fatalf("grant reservation audit = %#v, %v", reserve, err)
	}

	value, err := service.CallTool(ctx, uplink, "promote_retry", json.RawMessage(`{"id":"`+originalID+`"}`))
	if err != nil {
		t.Fatalf("promote_retry with the original grant: %v", err)
	}
	retried := value.(api.PromoteRetryResponse)
	delivered, err := fixture.store.Promotion(ctx, retried.ID)
	if err != nil || delivered.Status != model.PromotionPassed || delivered.GrantID != grant.ID || retried.RetryOf != originalID {
		t.Fatalf("retried promotion = %#v (%#v), %v, want delivered under the same grant", delivered, retried, err)
	}
	consumed, err := fixture.store.PromotionGrant(ctx, grant.ID)
	if err != nil || consumed.ConsumedAt == nil || consumed.ConsumedBy != delivered.ID {
		t.Fatalf("grant after delivery = %#v, %v, want consumed by %s", consumed, err, delivered.ID)
	}
	if consume, err := fixture.store.LatestAuditActionByType(ctx, "promotion_grant.consume", "promotion_grant"); err != nil || !strings.Contains(consume.Details, delivered.ID) {
		t.Fatalf("grant consumption audit = %#v, %v", consume, err)
	}

	// Still single-use: the delivered grant authorizes nothing further.
	if _, err := service.CallTool(ctx, uplink, "promote", json.RawMessage(`{"sha":"`+retrySourceSHA+`","branch":"main"}`)); !errors.Is(err, ErrForbidden) {
		t.Fatalf("promote after delivery error = %v, want ErrForbidden", err)
	}
	if _, err := service.CallTool(ctx, uplink, "promote_retry", json.RawMessage(`{"id":"`+originalID+`"}`)); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("second retry of a delivered tuple error = %v, want ErrInvalidInput", err)
	}
}

// A grant backs one promotion at a time: while one holds it, a second promote
// of the same tuple is refused and names the holder.
func TestHeldPromotionGrantRefusesASecondPromotion(t *testing.T) {
	git := &scriptedPromotionGit{plans: []gitcache.MergeCandidate{{BaseSHA: retryBaseSHA, MergedSHA: retryMergedSHA}}}
	fixture, _, service := newPromotionRetryHarness(t, git, passedPromotionCI)
	ctx := context.Background()
	admin := api.Actor{Identity: "admin@host", Fingerprint: "SHA256:admin", Admin: true}
	uplink := api.Actor{Identity: "agent@host", Fingerprint: "SHA256:agent"}
	if _, err := service.CallTool(ctx, admin, "promote_grant",
		json.RawMessage(`{"repo":"oberth","sha":"`+retrySourceSHA+`","branch":"main","expires":"1h"}`)); err != nil {
		t.Fatal(err)
	}
	pendingID := promoteForRetry(t, service, uplink)
	if pending, _ := fixture.store.Promotion(ctx, pendingID); pending.Status != model.PromotionPending {
		t.Fatalf("first promotion = %#v, want pending CI", pending)
	}
	_, err := service.CallTool(ctx, uplink, "promote", json.RawMessage(`{"sha":"`+retrySourceSHA+`","branch":"main"}`))
	if !errors.Is(err, ErrForbidden) || !strings.Contains(err.Error(), pendingID) {
		t.Fatalf("second promote while the grant is held error = %v, want ErrForbidden naming %s", err, pendingID)
	}
	if _, err := service.CallTool(ctx, uplink, "promote_retry", json.RawMessage(`{"id":"`+pendingID+`"}`)); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("retry of a pending promotion error = %v, want ErrInvalidInput", err)
	}
}

// publish_retry never re-pushes a promotion: by promotion ID or by its CI
// run's ID it refuses without changing state and points at promote_retry,
// naming the head the target moved to.
func TestPublishRetryRefusesPromotionsAndPointsAtPromoteRetry(t *testing.T) {
	git := &scriptedPromotionGit{
		plans:  []gitcache.MergeCandidate{{BaseSHA: retryBaseSHA, MergedSHA: retryMergedSHA}},
		pushes: []scriptedPush{{err: errors.New("rejected"), moveTo: retryMovedSHA}},
	}
	fixture, scheduler, service := newPromotionRetryHarness(t, git, passedPromotionCI)
	scheduler.SetPromotionRetrier(nil)
	admin := api.Actor{Identity: "agent@host", Admin: true}
	id := promoteForRetry(t, service, admin)
	if err := scheduler.ProcessNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	failed, err := fixture.store.Promotion(context.Background(), id)
	if err != nil || failed.Status != model.PromotionFailed || failed.RunID == "" {
		t.Fatalf("failed promotion = %#v, %v", failed, err)
	}
	for _, target := range []string{failed.ID, failed.RunID} {
		_, err := service.CallTool(context.Background(), admin, "publish_retry", json.RawMessage(`{"id":"`+target+`"}`))
		if !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), "promote_retry") || !strings.Contains(err.Error(), retryMovedSHA) {
			t.Fatalf("publish_retry %s error = %v, want a promote_retry pointer naming %s", target, err, retryMovedSHA)
		}
	}
	if again, _ := fixture.store.Promotion(context.Background(), id); again.Status != model.PromotionFailed {
		t.Fatalf("promotion after refused publish_retry = %#v", again)
	}
	if run, _ := fixture.store.Run(context.Background(), failed.RunID); run.Status != model.RunFailed || run.Phase != "publishing" {
		t.Fatalf("promotion run after refused publish_retry = %#v", run)
	}
	if _, pushed := git.counts(); pushed != 1 {
		t.Fatalf("pushes = %d, want only the original", pushed)
	}
}

// The automatic retry re-resolves the original uplink's authority: an admin
// that lost its admin flag, or an uplink that was removed, is not re-promoted
// with the authority it no longer has.
func TestMovedTargetRetryRefusesRevokedAuthority(t *testing.T) {
	for name, revoke := range map[string]func(*stubUplinkDirectory){
		"admin flag revoked": func(directory *stubUplinkDirectory) {
			directory.set("agent@host", model.Uplink{Identity: "agent@host", Fingerprint: "SHA256:agent"})
		},
		"uplink removed": func(directory *stubUplinkDirectory) { directory.set("agent@host") },
		"scope narrowed": func(directory *stubUplinkDirectory) {
			directory.set("agent@host", model.Uplink{Identity: "agent@host", Fingerprint: "SHA256:agent", Admin: true, Scope: []string{"github"}})
		},
	} {
		t.Run(name, func(t *testing.T) {
			git := &scriptedPromotionGit{
				plans: []gitcache.MergeCandidate{
					{BaseSHA: retryBaseSHA, MergedSHA: retryMergedSHA},
					{BaseSHA: retryMovedSHA, MergedSHA: retryMerged2},
				},
				pushes: []scriptedPush{{err: errors.New("rejected"), moveTo: retryMovedSHA}},
			}
			fixture, scheduler, service, uplinks := newPromotionRetryHarnessWithUplinks(t, git, passedPromotionCI)
			admin := api.Actor{Identity: "agent@host", Admin: true}
			id := promoteForRetry(t, service, admin)
			revoke(uplinks)
			if err := scheduler.ProcessNext(context.Background()); err != nil {
				t.Fatal(err)
			}
			service.waitBackground()
			status := promoteStatus(t, service, admin, id)
			if status.Promotion.Status != model.PromotionFailed || status.RetriedAs != "" {
				t.Fatalf("moved promotion after its authority went = %#v, want failed and not retried", status)
			}
			if prepared, _ := git.counts(); prepared != 1 {
				t.Fatalf("merge preparations = %d, want only the original", prepared)
			}
			if lineage, err := fixture.store.PromotionLineage(context.Background(), id); err != nil || lineage.RetriedAs != "" {
				t.Fatalf("lineage = %#v, %v", lineage, err)
			}
		})
	}
}
