package service

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/artifacts"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/store"
)

var ErrEvidencePurged = errors.New("run evidence purged")

type RetentionPolicy struct {
	Age         time.Duration
	BudgetBytes int64
}

type RetentionStatus struct {
	LogsBytes      int64 `json:"logs_bytes"`
	ArtifactsBytes int64 `json:"artifacts_bytes"`
	FreeBytes      int64 `json:"free_bytes"`
	// DatabaseBytes is the live database plus its write-ahead log, measured
	// at the last sweep; backups refuse a snapshot above their size ceiling.
	DatabaseBytes int64 `json:"database_bytes"`
	// TimingPruned counts runs whose Workflow and step timing records the
	// last sweep removed (older than the newest store.RunTimingRetainedRuns).
	TimingPruned int64     `json:"timing_pruned,omitempty"`
	LastSweep    time.Time `json:"last_sweep,omitempty"`
	LastError    string    `json:"last_error,omitempty"`
}

// Retention shares the same protected, audited purge for timer and admin work.
// Policies apply to terminal runs; protected evidence can exceed the budgets.
type Retention struct {
	Database       *store.Store
	Logs           *runlog.Store
	Artifacts      *artifacts.Store
	LogPolicy      RetentionPolicy
	ArtifactPolicy RetentionPolicy
	KeepNewest     int
	DataRoot       string
	MutationGate   func(context.Context) error
	AcquirePurge   func(string) (func(), error)
	Now            func() time.Time
	mu             sync.Mutex
	statusMu       sync.Mutex
	status         RetentionStatus
	resourceRetry  bool // protected by mu; contention/pass exhaustion is an expected skip
}

func (r *Retention) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Retention) Purge(ctx context.Context, actor api.Actor, id, what, reason string) (model.EvidencePurge, error) {
	if !actor.Admin || actor.Scoped() {
		return model.EvidencePurge{}, refuse(ErrForbidden, "run_purge requires an unscoped admin uplink")
	}
	// The store's purge transaction and AcquirePurge serialize each run.
	// A manual request must not wait for a sweep of unrelated evidence.
	if err := ctx.Err(); err != nil {
		return model.EvidencePurge{}, err
	}
	run, err := r.Database.ResolveRunID(ctx, id)
	if err != nil {
		return model.EvidencePurge{}, err
	}
	return r.purge(ctx, actor.Identity, run.ID, what, reason, 0)
}

func (r *Retention) purge(ctx context.Context, actor, id, what, reason string, keep int) (model.EvidencePurge, error) {
	return r.purgeWithMode(ctx, actor, id, what, reason, keep, false)
}

func (r *Retention) purgeWithMode(ctx context.Context, actor, id, what, reason string, keep int, automatic bool) (model.EvidencePurge, error) {
	if err := ctx.Err(); err != nil {
		return model.EvidencePurge{}, err
	}
	if r.MutationGate != nil {
		if err := r.MutationGate(ctx); err != nil {
			return model.EvidencePurge{}, fmt.Errorf("%w: audit integrity is unavailable: %w", ErrUnavailable, err)
		}
	}
	if r.AcquirePurge != nil {
		release, err := r.AcquirePurge(id)
		if err != nil {
			return model.EvidencePurge{}, err
		}
		defer release()
	}
	if automatic {
		// A sweep can have selected this run before a manual purge won its
		// permit. A completed marker is immutable evidence of that removal.
		previous, err := r.Database.RunEvidencePurges(ctx, id)
		if err != nil {
			return model.EvidencePurge{}, err
		}
		for _, value := range previous {
			if value.State == "complete" && (value.What == what || value.What == "both") {
				value.Bytes = 0
				return value, nil
			}
		}
	}
	return r.Database.PurgeRunEvidence(ctx, actor, id, what, reason, keep, func() (int64, error) {
		var bytes int64
		var errs []error
		if what == "logs" || what == "both" {
			n, err := r.Logs.Purge(id)
			bytes += n
			errs = append(errs, err)
		}
		if what == "artifacts" || what == "both" {
			n, err := r.Artifacts.Purge(id)
			bytes += n
			errs = append(errs, err)
		}
		return bytes, errors.Join(errs...)
	})
}

func (r *Retention) EvidenceError(ctx context.Context, id, what string) error {
	values, err := r.Database.RunEvidencePurges(ctx, id)
	if err != nil {
		return err
	}
	for index := len(values) - 1; index >= 0; index-- {
		value := values[index]
		if value.State == "refused" || (value.What != what && value.What != "both") {
			continue
		}
		if value.State == "pending" || value.State == "failed" {
			return fmt.Errorf("%w: %s purge requested by %s on %s (completion unconfirmed)", ErrEvidencePurged, what, value.Actor, value.At.Format(time.RFC3339))
		}
		return fmt.Errorf("%w: %s purged by %s on %s", ErrEvidencePurged, what, value.Actor, value.At.Format(time.RFC3339))
	}
	return nil
}

func (r *Retention) Sweep(ctx context.Context) (resultErr error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.statusMu.Lock()
	status := r.status
	r.statusMu.Unlock()
	defer func() {
		status.LastSweep = r.now().UTC()
		status.LastError = ""
		if resultErr != nil {
			status.LastError = resultErr.Error()
		}
		status.FreeBytes = statfsFreeBytes(r.DataRoot)
		status.DatabaseBytes = r.Database.DatabaseBytes()
		r.statusMu.Lock()
		r.status = status
		r.statusMu.Unlock()
	}()
	// Resource telemetry is independently bounded even with log/artifact
	// retention disabled. Its sweeper uses a separate SQLite connection.
	defer func() { resultErr = errors.Join(resultErr, r.pruneResourceUsage(ctx)) }()
	// Run timing detail is bounded by count whatever the evidence policies say.
	defer func() {
		pruned, err := r.Database.PruneRunTiming(ctx, store.RunTimingRetainedRuns, r.KeepNewest)
		status.TimingPruned = pruned
		if store.IsTransientLockError(err) || (errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil) {
			err = nil // contention or an exhausted pass budget; the next sweep continues
		}
		resultErr = errors.Join(resultErr, err)
	}()
	logs, err := r.Logs.UsageByRun()
	if err != nil {
		return err
	}
	artifacts, err := r.Artifacts.UsageByRun()
	if err != nil {
		return err
	}
	status.LogsBytes, status.ArtifactsBytes = 0, 0
	for _, bytes := range logs {
		status.LogsBytes += bytes
	}
	for _, bytes := range artifacts {
		status.ArtifactsBytes += bytes
	}
	if r.LogPolicy.Age <= 0 && r.LogPolicy.BudgetBytes <= 0 && r.ArtifactPolicy.Age <= 0 && r.ArtifactPolicy.BudgetBytes <= 0 {
		return nil
	}
	// Take one read-only protection snapshot for both evidence classes. Only
	// eligible candidates enter the audited purge's immediate write transaction.
	known, err := r.Database.EvidenceRetentionRuns(ctx, r.KeepNewest)
	if err != nil {
		return err
	}
	for _, item := range []struct {
		what   string
		usage  map[string]int64
		policy RetentionPolicy
		total  *int64
	}{
		{"logs", logs, r.LogPolicy, &status.LogsBytes},
		{"artifacts", artifacts, r.ArtifactPolicy, &status.ArtifactsBytes},
	} {
		if item.policy.Age <= 0 && item.policy.BudgetBytes <= 0 {
			continue
		}
		var runs []store.EvidenceRetentionRun
		for id := range item.usage {
			run, exists := known[id]
			if !exists {
				// Orphans have no retention timestamp or recoverable run history.
				// Reclaim them whenever this evidence class has an enabled policy.
				runs = append(runs, store.EvidenceRetentionRun{ID: id})
			} else if !run.Protected && run.FinishedAt != nil {
				runs = append(runs, run)
			}
		}
		sort.Slice(runs, func(i, j int) bool {
			if runs[i].FinishedAt == nil || runs[j].FinishedAt == nil {
				if runs[i].FinishedAt == nil && runs[j].FinishedAt == nil {
					return runs[i].ID < runs[j].ID
				}
				return runs[i].FinishedAt == nil
			}
			if runs[i].FinishedAt.Equal(*runs[j].FinishedAt) {
				return runs[i].QueueSequence < runs[j].QueueSequence
			}
			return runs[i].FinishedAt.Before(*runs[j].FinishedAt)
		})
		for _, run := range runs {
			orphan := run.FinishedAt == nil
			expired := !orphan && item.policy.Age > 0 && !run.FinishedAt.After(r.now().Add(-item.policy.Age))
			overBudget := item.policy.BudgetBytes > 0 && *item.total > item.policy.BudgetBytes
			if !orphan && !expired && !overBudget {
				continue
			}
			reason := "retention policy"
			if orphan {
				reason = "orphaned evidence"
			}
			value, err := r.purgeSweepCandidate(ctx, run.ID, item.what, reason, r.KeepNewest)
			if errors.Is(err, store.ErrEvidenceProtected) {
				continue
			}
			*item.total -= value.Bytes
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// Status returns the last sweep's measurements without walking evidence trees.
func (r *Retention) Status(ctx context.Context) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.statusMu.Lock()
	defer r.statusMu.Unlock()
	return r.status, nil
}

// pruneResourceUsage is called with mu held. SQLite contention and exhaustion
// of the store's bounded pass are skips. Run schedules a short retry while the
// caller remains live; cancellation and other failures still propagate.
func (r *Retention) pruneResourceUsage(ctx context.Context) error {
	_, err := r.Database.PruneResourceUsage(ctx)
	r.resourceRetry = ctx.Err() == nil && (store.IsTransientLockError(err) || errors.Is(err, context.DeadlineExceeded))
	if r.resourceRetry {
		return nil
	}
	return err
}

func (r *Retention) Run(ctx context.Context, interval time.Duration, logger *log.Logger) {
	r.run(ctx, interval, min(interval, time.Minute), logger)
}

func (r *Retention) run(ctx context.Context, interval, retryDelay time.Duration, logger *log.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	retry := time.NewTimer(retryDelay)
	retry.Stop()
	defer retry.Stop()
	armRetry := func() {
		r.mu.Lock()
		pending := r.resourceRetry
		r.mu.Unlock()
		retry.Stop()
		if pending && ctx.Err() == nil {
			logger.Printf("DEBUG: resource telemetry retention busy or pass budget exhausted; skipped, retry in %s", retryDelay)
			retry.Reset(retryDelay)
		}
	}
	sweep := func() {
		if err := r.Sweep(ctx); err != nil && ctx.Err() == nil {
			logger.Printf("retention sweep: %v", err)
		}
		armRetry()
	}
	sweep()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweep()
		case <-retry.C:
			r.mu.Lock()
			err := r.pruneResourceUsage(ctx)
			r.mu.Unlock()
			if err != nil && ctx.Err() == nil {
				r.statusMu.Lock()
				r.status.LastError = err.Error()
				r.statusMu.Unlock()
				logger.Printf("resource telemetry retention retry: %v", err)
			}
			armRetry()
		}
	}
}

func (service *API) purgedEvidence(ctx context.Context, run model.Run, what string) error {
	if service.retention == nil || !run.Status.Terminal() {
		return nil
	}
	return service.retention.EvidenceError(ctx, run.ID, what)
}

func (r *Retention) purgeSweepCandidate(ctx context.Context, id, what, reason string, keep int) (model.EvidencePurge, error) {
	return r.purgeWithMode(ctx, "oberth-retention", id, what, reason, keep, true)
}
