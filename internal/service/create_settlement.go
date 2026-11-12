package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/pkg/pipeline"
)

const createObservationWindow = time.Minute
const createSettlementAttemptBudget = 15 * time.Second
const pendingCreateMessage = "Workflow create outcome uncertain; cleanup pending"

// createAdmission binds a pending Workflow create to its run. Size is the
// run's historical tier, written for older readers of the file; the gate
// counts runs and reads nothing else from it.
type createAdmission struct {
	RunID, WorkflowName, SHA string
	RepoID                   int64
	Size                     pipeline.Size
}

type pendingCreate struct {
	runID       string
	reason      string
	grant       *admissionReservation
	persisted   bool
	confirmed   bool
	next, since time.Time
	attempts    int
}

func isPendingCreate(run model.Run) bool {
	return run.Phase == "infrastructure-pending" || strings.HasPrefix(run.Error, pendingCreateMessage) || store.WorkflowCleanupReason(run.Error)
}
func (scheduler *Scheduler) createTime() time.Time {
	if scheduler.createNow != nil {
		return scheduler.createNow()
	}
	return time.Now()
}
func (scheduler *Scheduler) createAdmissionPath(id string) string {
	return runCreateAdmissionPath(scheduler.workspaceRoot, id)
}

func runCreateAdmissionPath(root, id string) string {
	return filepath.Join(root, ".create-admissions", id+".json")
}

func (scheduler *Scheduler) recordWorkflowCreateBinding(run model.Run, name string) error {
	target := scheduler.createAdmissionPath(run.ID)
	if !pathWithin(scheduler.workspaceRoot, target) {
		return errors.New("service: create admission escaped workspace")
	}
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		return err
	}
	body, err := json.Marshal(createAdmission{RunID: run.ID, WorkflowName: name, SHA: run.SHA, RepoID: run.RepoID, Size: pipeline.M})
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(target), ".create-admission-")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(file.Name()) }()
	if _, err := file.Write(body); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), target); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(target))
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}
func (scheduler *Scheduler) readCreateAdmission(run model.Run) (createAdmission, error) {
	file, err := os.Open(scheduler.createAdmissionPath(run.ID))
	if err != nil {
		return createAdmission{}, err
	}
	defer func() { _ = file.Close() }()
	body, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil {
		return createAdmission{}, err
	}
	var saved createAdmission
	if len(body) > 4096 || json.Unmarshal(body, &saved) != nil || saved.RunID != run.ID || saved.WorkflowName != run.WorkflowName || saved.RepoID != run.RepoID || saved.SHA != run.SHA || !saved.Size.Valid() || saved.Size != saved.Size.Effective() {
		return createAdmission{}, errors.New("service: pending create admission binding mismatch")
	}
	return saved, nil
}

// restoreCreateGrant only restores local admission accounting.
func (scheduler *Scheduler) restoreCreateGrant(run model.Run, grant *admissionReservation) (*admissionReservation, error) {
	if grant == nil {
		scheduler.admission.mu.Lock()
		existing := scheduler.admission.reservations[run.QueueSequence]
		if existing != nil && existing.runID == run.ID && existing.granted {
			grant = existing
		}
		scheduler.admission.mu.Unlock()
	}
	if grant == nil {
		if _, err := scheduler.readCreateAdmission(run); err != nil {
			// A missing or corrupt binding changes nothing about the slot the
			// run holds; keep the API available and warn.
			log.Printf("WARN: create_cleanup run=%q missing_admission_contract=true", run.ID)
		}
		var err error
		grant, err = scheduler.admission.adopt(run)
		if err != nil {
			return nil, err
		}
	}
	scheduler.admission.mu.Lock()
	grant.retained = true
	grant.submitted = false
	scheduler.admission.mu.Unlock()
	return grant, nil
}

// registerCreateCleanup never waits for Kubernetes, including on startup,
// push and cancellation paths. Failed local restores are owned by the background task.
func (scheduler *Scheduler) registerCreateCleanup(run model.Run, grant *admissionReservation, persisted bool) {
	// Serialize local restoration and registration so concurrent wake/cancel
	// callers cannot repeat its admission side effects.
	scheduler.createMu.Lock()
	defer scheduler.createMu.Unlock()
	if _, exists := scheduler.createPending[run.ID]; exists {
		return
	}
	restored, err := scheduler.restoreCreateGrant(run, grant)
	if err != nil {
		log.Printf("WARN: create_cleanup run=%q restore_conflict=true deferred=true", run.ID)
	}
	now := scheduler.createTime()
	if scheduler.createPending == nil {
		scheduler.createPending = make(map[string]*pendingCreate)
	}
	// A nil grant is an owned, retryable restore failure, never a dropped run.
	reason := ""
	if store.WorkflowCleanupReason(run.Error) {
		reason = run.Error
	}
	scheduler.createPending[run.ID] = &pendingCreate{runID: run.ID, reason: reason, grant: restored, persisted: persisted, next: now, since: now}
	scheduler.signalCreateCleanup()
}
func (scheduler *Scheduler) signalCreateCleanup() {
	select {
	case scheduler.createWake <- struct{}{}:
	default:
	}
}
func (scheduler *Scheduler) ownsCreateCleanup(id string) bool {
	scheduler.createMu.Lock()
	defer scheduler.createMu.Unlock()
	_, ok := scheduler.createPending[id]
	return ok
}
func createSettlementBackoff(attempt int) time.Duration {
	return min(5*time.Second*time.Duration(1<<min(max(attempt-1, 0), 6)), 5*time.Minute)
}

// One shared background task, one due run per tick. Per-attempt API/DB work is bounded;
// retries have no total deadline. An outage holds that grant, never the server.
func (scheduler *Scheduler) runCreateCleanup(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	ticks := ticker.C
	if scheduler.createTicks != nil {
		ticks = scheduler.createTicks
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
		case <-scheduler.createWake:
		}
		scheduler.settleOneCreate(ctx)
		scheduler.sweepAssistHolds(ctx)
		if cleanup, ok := scheduler.jobs.(interface{ ReconcileTargetCleanup(context.Context) error }); ok {
			attempt, cancel := context.WithTimeout(ctx, createSettlementAttemptBudget)
			if err := scheduler.requireMutation(attempt); err == nil {
				if err := cleanup.ReconcileTargetCleanup(attempt); err != nil {
					log.Print("WARN: execution target cleanup remains pending")
				}
			}
			cancel()
		}
	}
}
func (scheduler *Scheduler) settleOneCreate(ctx context.Context) {
	now := scheduler.createTime()
	scheduler.createMu.Lock()
	due := make([]*pendingCreate, 0, len(scheduler.createPending))
	for _, pending := range scheduler.createPending {
		if !pending.next.After(now) {
			due = append(due, pending)
		}
	}
	sort.Slice(due, func(i, j int) bool {
		if due[i].next.Equal(due[j].next) {
			return due[i].runID < due[j].runID
		}
		return due[i].next.Before(due[j].next)
	})
	if len(due) == 0 {
		scheduler.createMu.Unlock()
		return
	}
	pending := due[0]
	scheduler.createMu.Unlock()
	attemptCtx, cancel := context.WithTimeout(ctx, createSettlementAttemptBudget)
	done, err := scheduler.settleCreateAttempt(attemptCtx, pending)
	cancel()
	if ctx.Err() != nil {
		return
	}
	scheduler.createMu.Lock()
	if done {
		delete(scheduler.createPending, pending.runID)
		scheduler.createMu.Unlock()
		scheduler.admission.mu.Lock()
		pending.grant.retained = false
		scheduler.admission.releaseLocked(pending.grant)
		scheduler.admission.mu.Unlock()
		scheduler.signalQueue()
		return
	}
	pending.attempts++
	delay := createSettlementBackoff(pending.attempts)
	pending.next = scheduler.createTime().Add(delay)
	age := scheduler.createTime().Sub(pending.since)
	scheduler.createMu.Unlock()
	reason := "unconfirmed"
	if err != nil {
		reason = "observation_failed"
		if errors.Is(err, context.DeadlineExceeded) {
			reason = "deadline"
		}
	}
	log.Printf("WARN: create_cleanup run=%q pending_for=%s attempt=%d reason=%s retry_in=%s", pending.runID, age, pending.attempts, reason, delay)
}
func (scheduler *Scheduler) settleCreateAttempt(ctx context.Context, pending *pendingCreate) (bool, error) {
	// This reconciler owns its own bounded attempts and backoff. Nested DB
	// adapters must not acquire the scheduler's unbounded retry continuation.
	ctx = context.WithValue(ctx, databaseRetryKey{}, (*databaseRetryScope)(nil))
	if err := scheduler.requireMutation(ctx); err != nil {
		return false, err
	}
	run, err := scheduler.store.Run(ctx, pending.runID)
	if err != nil {
		return false, err
	}
	if !pending.persisted {
		if err := scheduler.store.MarkRunCreatePending(ctx, run.ID, run.WorkflowName, pending.reason); err != nil {
			return false, err
		}
		pending.persisted = true
	}
	if pending.grant == nil {
		grant, err := scheduler.restoreCreateGrant(run, nil)
		if err != nil {
			return false, err
		}
		scheduler.createMu.Lock()
		pending.grant = grant
		scheduler.createMu.Unlock()
	}
	cancellations, err := scheduler.store.PendingRunCancellations(ctx)
	if err != nil {
		return false, err
	}
	var observed *model.RunCancellation
	for _, c := range cancellations {
		if c.RunID == run.ID {
			copy := c
			observed = &copy
			break
		}
	}
	if observed != nil {
		// A re-arm supersedes any previous final observation.
		pending.confirmed = false
		settler, ok := scheduler.jobs.(JobCreateSettler)
		if !ok {
			return false, errors.New("service: engine cannot settle uncertain create")
		}
		done, err := settler.SettleCreate(ctx, observed.WorkflowName, run.ID, observed.CreatedAt.Add(createObservationWindow))
		if err != nil || !done {
			return false, err
		}
		if err := scheduler.store.CompleteObservedRunCancellation(ctx, *observed); err != nil {
			return false, err
		}
		pending.confirmed = true
	}
	if !pending.confirmed {
		// A prior owner may have committed completion but lost finalization.
		// Re-arm one observation window rather than wedging the grant forever.
		if err := scheduler.store.MarkRunCreatePending(ctx, run.ID, run.WorkflowName); err != nil {
			return false, err
		}
		return false, nil
	}
	run, err = scheduler.store.Run(ctx, run.ID)
	if err != nil {
		return false, err
	}
	if run.Status == model.RunRunning {
		message := "workflow create outcome uncertain; cleanup confirmed absence"
		switch run.Error {
		case store.WorkflowSpecMismatchPendingMessage:
			message = "Workflow spec mismatch; cleanup confirmed absence"
		case store.ExecutorIdentityPendingMessage:
			message = "executor identity readiness deadline exceeded after one issuance retry; cleanup confirmed absence"
		case store.AdmissionStalledPendingMessage:
			message = "resource admission expired: no scheduled pod or progress; cleanup confirmed absence"
		}
		if err := scheduler.finishInfrastructureFailure(ctx, run, model.Repository{}, errors.New(message)); err != nil {
			return false, err
		}
	}
	// Do not release against a concurrent cancellation re-arm.
	cancellations, err = scheduler.store.PendingRunCancellations(ctx)
	if err != nil {
		return false, err
	}
	for _, c := range cancellations {
		if c.RunID == run.ID {
			pending.confirmed = false
			return false, nil
		}
	}
	// Exact external absence and the durable verdict are established. Scratch
	// cleanup can retry independently; it must not keep CPU/memory reserved.
	pending.grant.releaseAfterCleanup()
	cleanupCtx, cancel := context.WithTimeout(ctx, workspaceCleanupTimeout)
	err = scheduler.workspaces.cleanupRun(cleanupCtx, run.ID)
	cancel()
	if err != nil {
		return false, err
	}
	log.Printf("create_cleanup run=%q settled=true", run.ID)
	return true, nil
}

// The shared background owner retains capacity during uncertain create/delete
// outcomes; API outages must not turn this run's cleanup into a component exit.
func (scheduler *Scheduler) deferCreateCleanup(ctx context.Context, run model.Run, name string, grant *admissionReservation, failures ...error) {
	grant.retain()
	reason := ""
	for _, failure := range failures {
		var expired interface{ AdmissionExpiryReason() string }
		if errors.As(failure, &expired) {
			switch expired.AdmissionExpiryReason() {
			case "identity":
				reason = store.ExecutorIdentityPendingMessage
			case "stalled":
				reason = store.AdmissionStalledPendingMessage
			}
			run.Error = reason
		}
		var mismatch interface{ WorkflowSpecMismatch() bool }
		if errors.As(failure, &mismatch) && mismatch.WorkflowSpecMismatch() {
			reason = store.WorkflowSpecMismatchPendingMessage
			run.Error = reason
		}
	}
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	err := scheduler.database().MarkRunCreatePending(persistCtx, run.ID, name, reason)
	cancel()
	run.WorkflowName = name
	scheduler.registerCreateCleanup(run, grant, err == nil)
	if err != nil {
		log.Printf("WARN: create_cleanup run=%q persistence_deferred=true", run.ID)
	}
}

// sweepAssistHolds abandons parked assist attempts past the hold TTL or
// above the installation cap. Pods their agent already continued or
// abandoned (the run's assist record) are running work and are skipped; if
// that record cannot be read, nothing is abandoned, so a released step is
// never killed by a blind sweep; each victim's record is read again right
// before its delete. Each abandon is appended to its run's
// assist record and audited as run.assist.abandon with actor oberth.
func (scheduler *Scheduler) sweepAssistHolds(ctx context.Context) {
	holds, ok := scheduler.jobs.(interface {
		EnforceAssistHolds(context.Context, map[string]model.AssistPodActivity, model.AssistActivityRecheck, time.Time) ([]model.AssistAbandon, error)
	})
	if !ok {
		return
	}
	attempt, cancel := context.WithTimeout(ctx, createSettlementAttemptBudget)
	defer cancel()
	activity := map[string]model.AssistPodActivity{}
	if source, ok := scheduler.store.(interface {
		AssistPodActivity(context.Context) (map[string]model.AssistPodActivity, error)
	}); ok {
		var err error
		if activity, err = source.AssistPodActivity(attempt); err != nil {
			log.Print("WARN: assist hold sweep skipped: released holds unknown")
			return
		}
	}
	// The snapshot above predates the Pod list; each victim is re-read
	// from its run right before the delete, so a continue or a worked
	// action landing in between spares it. An unreadable run spares it too.
	recheck := func(ctx context.Context, runID, pod string) (model.AssistPodActivity, error) {
		if runID == "" {
			return model.AssistPodActivity{}, nil
		}
		run, err := scheduler.store.Run(ctx, runID)
		if err != nil {
			return model.AssistPodActivity{}, err
		}
		if run.Assist == nil {
			return model.AssistPodActivity{}, nil
		}
		return run.Assist.PodActivities()[pod], nil
	}
	abandoned, err := holds.EnforceAssistHolds(attempt, activity, recheck, time.Now())
	if err != nil {
		log.Print("WARN: assist hold sweep remains pending")
	}
	recorder, _ := scheduler.store.(interface {
		AppendRunAssist(context.Context, model.RunAssistRequest) (model.Run, error)
	})
	for _, abandon := range abandoned {
		log.Printf("assist: abandoned hold run=%s step=%s pod=%s reason=%s held_for=%s", abandon.RunID, abandon.Step, abandon.Pod, abandon.Reason, abandon.AbandonedAt.Sub(abandon.HeldSince).Round(time.Second))
		if recorder == nil || abandon.RunID == "" {
			continue
		}
		if _, err := recorder.AppendRunAssist(attempt, model.RunAssistRequest{RunID: abandon.RunID, Event: abandon.Event()}); err != nil {
			log.Printf("WARN: assist abandon of run=%s pod=%s not recorded", abandon.RunID, abandon.Pod)
		} else {
			scheduler.signals.NotifyRun(abandon.RunID)
		}
	}
}
