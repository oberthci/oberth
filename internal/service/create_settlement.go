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
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

const createObservationWindow = time.Minute
const createSettlementAttemptBudget = 15 * time.Second
const pendingCreateMessage = "Workflow create outcome uncertain; cleanup pending"

type createAdmission struct {
	RunID, JobName, SHA, Group string
	RepoID                     int64
	Size                       periapsis.Size
	Demand                     argoworkflow.VMIDemand
}

type pendingCreate struct {
	runID       string
	grant       *admissionReservation
	persisted   bool
	confirmed   bool
	next, since time.Time
	attempts    int
}

func isPendingCreate(run model.Run) bool {
	return run.Phase == "infrastructure-pending" || strings.HasPrefix(run.Error, pendingCreateMessage)
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

// Persist the server-admitted contract before POST. Recovery must never need a
// Kubernetes read, a moving repository ref or a remote fragment to restore it.
func (scheduler *Scheduler) recordCreateAdmission(run model.Run, name string, size periapsis.Size, demand argoworkflow.VMIDemand) error {
	target := scheduler.createAdmissionPath(run.ID)
	if !pathWithin(scheduler.workspaceRoot, target) {
		return errors.New("service: create admission escaped workspace")
	}
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		return err
	}
	body, err := json.Marshal(createAdmission{RunID: run.ID, JobName: name, SHA: run.SHA, RepoID: run.RepoID, Group: run.ConcurrencyGroup, Size: size.Effective(), Demand: demand})
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
	if len(body) > 4096 || json.Unmarshal(body, &saved) != nil || saved.RunID != run.ID || saved.JobName != run.JobName || saved.RepoID != run.RepoID || saved.SHA != run.SHA || saved.Group != run.ConcurrencyGroup || !saved.Size.Valid() || saved.Size != saved.Size.Effective() {
		return createAdmission{}, errors.New("service: pending create admission binding mismatch")
	}
	if err := (KubeVirtResources{Demand: saved.Demand, Limit: argoworkflow.VMIBudgetLimit().DemandLimit()}).Validate(); err != nil {
		return createAdmission{}, err
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
		saved, err := scheduler.readCreateAdmission(run)
		if err != nil {
			// Pre-feature/corrupt snapshots have no trustworthy resource contract.
			// Keep the API available, conservatively reserve the ceiling, and warn.
			saved.Size = periapsis.XL
			saved.Demand = argoworkflow.VMIBudgetLimit().DemandLimit()
			log.Printf("WARN: create_cleanup run=%q missing_admission_contract=true reserving_ceiling=true", run.ID)
		}
		grant, err = scheduler.admission.adopt(run, saved.Size, saved.Demand)
		if err != nil {
			// Conflicting historical groups are quarantined behind a full budget.
			run.ConcurrencyGroup = ""
			grant, err = scheduler.admission.adopt(run, periapsis.XL, argoworkflow.VMIBudgetLimit().DemandLimit())
			if err != nil {
				return nil, err
			}
			scheduler.admission.mu.Lock()
			scheduler.admission.used += scheduler.admission.capacity - grant.weight
			grant.weight = scheduler.admission.capacity
			scheduler.admission.mu.Unlock()
		}
	}
	scheduler.admission.mu.Lock()
	grant.retained = true
	grant.submitted = false
	scheduler.admission.mu.Unlock()
	return grant, nil
}

// registerCreateCleanup never waits for Kubernetes, including on startup,
// push and cancellation paths. Failed local restores are owned by the worker.
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
	scheduler.createPending[run.ID] = &pendingCreate{runID: run.ID, grant: restored, persisted: persisted, next: now, since: now}
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

// One shared worker, one due run per tick. Per-attempt API/DB work is bounded;
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
		if err := scheduler.store.MarkRunCreatePending(ctx, run.ID, run.JobName); err != nil {
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
		done, err := settler.SettleCreate(ctx, observed.JobName, run.ID, observed.CreatedAt.Add(createObservationWindow))
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
		if err := scheduler.store.MarkRunCreatePending(ctx, run.ID, run.JobName); err != nil {
			return false, err
		}
		return false, nil
	}
	run, err = scheduler.store.Run(ctx, run.ID)
	if err != nil {
		return false, err
	}
	if run.Status == model.RunRunning {
		if err := scheduler.finishInfrastructureFailure(ctx, run, model.Repository{}, errors.New("workflow create outcome uncertain; cleanup confirmed absence")); err != nil {
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
	cleanupCtx, cancel := context.WithTimeout(ctx, workspaceCleanupTimeout)
	err = scheduler.workspaces.cleanupRun(cleanupCtx, run.ID)
	cancel()
	if err != nil {
		return false, err
	}
	log.Printf("create_cleanup run=%q settled=true", run.ID)
	return true, nil
}

// Kept separate for status/diagnostic callers without exposing API errors.
func (scheduler *Scheduler) pendingCreateCount() int {
	scheduler.createMu.Lock()
	defer scheduler.createMu.Unlock()
	return len(scheduler.createPending)
}

// The shared background owner retains capacity during uncertain create/delete
// outcomes; API outages must not turn this run's cleanup into a component exit.
func (scheduler *Scheduler) deferCreateCleanup(ctx context.Context, run model.Run, name string, grant *admissionReservation) {
	grant.retain()
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	err := scheduler.database().MarkRunCreatePending(persistCtx, run.ID, name)
	cancel()
	run.JobName = name
	scheduler.registerCreateCleanup(run, grant, err == nil)
	if err != nil {
		log.Printf("WARN: create_cleanup run=%q persistence_deferred=true", run.ID)
	}
}
