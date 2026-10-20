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
	LogsBytes      int64     `json:"logs_bytes"`
	ArtifactsBytes int64     `json:"artifacts_bytes"`
	FreeBytes      int64     `json:"free_bytes"`
	DiskFloorBytes int64     `json:"disk_floor_bytes"`
	BelowFloor     bool      `json:"below_floor"`
	LastSweep      time.Time `json:"last_sweep,omitempty"`
	LastError      string    `json:"last_error,omitempty"`
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
	DiskFloorBytes int64
	MutationGate   func(context.Context) error
	AcquirePurge   func(string) (func(), error)
	Now            func() time.Time
	mu             sync.Mutex
	statusMu       sync.Mutex
	lastSweep      time.Time
	lastError      string
}

func (r *Retention) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Retention) Purge(ctx context.Context, actor api.Actor, id, what, reason string) (model.EvidencePurge, error) {
	if !actor.Admin || actor.Scoped() {
		return model.EvidencePurge{}, fmt.Errorf("%w: run_purge requires an unscoped admin uplink", ErrForbidden)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	run, err := r.Database.ResolveRunID(ctx, id)
	if err != nil {
		return model.EvidencePurge{}, err
	}
	return r.purge(ctx, actor.Identity, run.ID, what, reason, 0)
}

func (r *Retention) purge(ctx context.Context, actor, id, what, reason string, keep int) (model.EvidencePurge, error) {
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
	defer func() {
		r.statusMu.Lock()
		defer r.statusMu.Unlock()
		r.lastSweep = r.now().UTC()
		r.lastError = ""
		if resultErr != nil {
			r.lastError = resultErr.Error()
		}
	}()
	logs, err := r.Logs.UsageByRun()
	if err != nil {
		return err
	}
	artifacts, err := r.Artifacts.UsageByRun()
	if err != nil {
		return err
	}
	for _, item := range []struct {
		what   string
		usage  map[string]int64
		policy RetentionPolicy
	}{
		{"logs", logs, r.LogPolicy}, {"artifacts", artifacts, r.ArtifactPolicy},
	} {
		if item.policy.Age <= 0 && item.policy.BudgetBytes <= 0 {
			continue
		}
		var total int64
		var runs []model.Run
		for id, bytes := range item.usage {
			total += bytes
			run, err := r.Database.Run(ctx, id)
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			if run.Status.Terminal() && run.FinishedAt != nil {
				runs = append(runs, run)
			}
		}
		sort.Slice(runs, func(i, j int) bool {
			if runs[i].FinishedAt.Equal(*runs[j].FinishedAt) {
				return runs[i].QueueSequence < runs[j].QueueSequence
			}
			return runs[i].FinishedAt.Before(*runs[j].FinishedAt)
		})
		for _, run := range runs {
			expired := item.policy.Age > 0 && !run.FinishedAt.After(r.now().Add(-item.policy.Age))
			overBudget := item.policy.BudgetBytes > 0 && total > item.policy.BudgetBytes
			if !expired && !overBudget {
				continue
			}
			value, err := r.purge(ctx, "oberth-retention", run.ID, item.what, "retention policy", r.KeepNewest)
			if errors.Is(err, store.ErrEvidenceProtected) {
				continue
			}
			total -= value.Bytes
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *Retention) Status(ctx context.Context) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	logs, err := r.Logs.UsageByRun()
	if err != nil {
		return nil, err
	}
	artifacts, err := r.Artifacts.UsageByRun()
	if err != nil {
		return nil, err
	}
	value := RetentionStatus{DiskFloorBytes: r.DiskFloorBytes, FreeBytes: statfsFreeBytes(r.DataRoot)}
	for _, bytes := range logs {
		value.LogsBytes += bytes
	}
	for _, bytes := range artifacts {
		value.ArtifactsBytes += bytes
	}
	value.BelowFloor = value.DiskFloorBytes > 0 && value.FreeBytes < value.DiskFloorBytes
	r.statusMu.Lock()
	value.LastSweep = r.lastSweep
	value.LastError = r.lastError
	r.statusMu.Unlock()
	return value, nil
}

func (r *Retention) Run(ctx context.Context, interval time.Duration, logger *log.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := r.Sweep(ctx); err != nil && ctx.Err() == nil {
			logger.Printf("retention sweep: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (service *API) purgedEvidence(ctx context.Context, run model.Run, what string) error {
	if service.retention == nil || !run.Status.Terminal() {
		return nil
	}
	return service.retention.EvidenceError(ctx, run.ID, what)
}
