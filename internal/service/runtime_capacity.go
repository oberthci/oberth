package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

const runtimeCapacityInterval = 5 * time.Second

func admissionCapacityBudget(jobs int, weights SizeWeights, requireXLFit bool) (int, error) {
	if jobs < 1 || jobs > 64 || weights.M < 1 || weights.M > math.MaxInt/jobs {
		return 0, errors.New("admission capacity requires 1..64 jobs and a representable jobs * M weight budget")
	}
	budget := jobs * weights.M
	if requireXLFit && weights.XL > budget {
		return 0, errors.New("admission capacity is below the configured XL weight")
	}
	return budget, nil
}

// Open the key on every sample: Kubernetes updates a projected ConfigMap by
// swapping symlinks. A retained descriptor or subPath mount would pin old bytes.
func readRuntimeCapacity(path string) (int, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, errors.New("capacity file is unreadable")
	}
	if !info.Mode().IsRegular() {
		return 0, errors.New("capacity file must be a regular file")
	}
	file, err := os.Open(path) // #nosec G304 -- operator-configured mounted key; symlink traversal is required for ConfigMap updates.
	if err != nil {
		return 0, errors.New("capacity file is unreadable")
	}
	defer func() { _ = file.Close() }()
	info, err = file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return 0, errors.New("capacity file must be a regular file")
	}
	body, err := io.ReadAll(io.LimitReader(file, 65))
	if err != nil {
		return 0, errors.New("capacity file is unreadable")
	}
	if len(body) > 64 {
		return 0, errors.New("capacity file exceeds 64 bytes")
	}
	text := strings.TrimSpace(string(body))
	if text == "" || strings.IndexFunc(text, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return 0, errors.New("capacity file requires one integer in 1..64")
	}
	jobs, err := strconv.Atoi(text)
	if err != nil || jobs < 1 || jobs > 64 {
		return 0, errors.New("capacity file requires one integer in 1..64")
	}
	return jobs, nil
}

func (scheduler *Scheduler) concurrentJobs() int {
	gate := scheduler.admission
	gate.mu.Lock()
	defer gate.mu.Unlock()
	return scheduler.concurrentJobsLocked()
}

// concurrentJobsLocked shares the effective-limit policy when a caller needs
// one coherent snapshot with other admission fields. Caller holds admission.mu.
func (scheduler *Scheduler) concurrentJobsLocked() int {
	gate := scheduler.admission
	if (gate.capacityConfig.Source == "file" || gate.capacityConfig.Source == "runtime") && gate.capacityConfig.MaxConcurrentJobs > 0 {
		return gate.capacityConfig.MaxConcurrentJobs
	}
	return scheduler.maxConcurrent
}

func (scheduler *Scheduler) capacityReloadError(err error) {
	gate := scheduler.admission
	gate.mu.Lock()
	changed := gate.capacityConfig.Error != err.Error()
	gate.capacityConfig.Error = err.Error()
	current := gate.capacityConfig.MaxConcurrentJobs
	gate.mu.Unlock()
	if changed {
		log.Printf("WARN: admission capacity: %v; retaining %d concurrent runs", err, current)
	}
}

// CapacityOverrideReader returns the server-owned runtime ceiling (#1497),
// or nil when none is set. It takes precedence over the mounted file and the
// boot flag; Helm never writes it, so upgrades cannot conflict with it.
type CapacityOverrideReader func(context.Context) (*model.AdmissionCapacityOverride, error)

// SetCapacityOverrideReader wires the runtime layer before Run.
func (scheduler *Scheduler) SetCapacityOverrideReader(read CapacityOverrideReader) {
	scheduler.capacityReloadMu.Lock()
	defer scheduler.capacityReloadMu.Unlock()
	scheduler.capacityOverride = read
}

// ValidateRuntimeCapacity applies the same bounds and weight budget check as
// a reload, so an administrator change that cannot apply is refused early.
func (scheduler *Scheduler) ValidateRuntimeCapacity(jobs int) error {
	if jobs < 1 || jobs > 64 {
		return errors.New("admission capacity requires 1..64 jobs")
	}
	gate := scheduler.admission
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if gate.workflowOnly {
		return nil
	}
	_, err := admissionCapacityBudget(jobs, gate.weights, true)
	return err
}

// ApplyRuntimeCapacity reloads every capacity layer now and returns the
// resulting configuration, including any retained-value error.
func (scheduler *Scheduler) ApplyRuntimeCapacity(ctx context.Context) model.AdmissionCapacityConfig {
	scheduler.refreshRuntimeCapacity(ctx)
	gate := scheduler.admission
	gate.mu.Lock()
	defer gate.mu.Unlock()
	return gate.capacityConfig
}

// Precedence: runtime override, then the mounted file, then the boot flag.
func (scheduler *Scheduler) desiredRuntimeCapacity(ctx context.Context) (int, string, error) {
	if scheduler.capacityOverride != nil {
		override, err := scheduler.capacityOverride(ctx)
		if err != nil {
			return 0, "", errors.New("runtime capacity override is unreadable")
		}
		if override != nil {
			return override.MaxConcurrentJobs, "runtime", nil
		}
	}
	if scheduler.capacityFile != "" {
		jobs, err := readRuntimeCapacity(scheduler.capacityFile)
		return jobs, "file", err
	}
	return scheduler.maxConcurrent, "boot", nil
}

// Failure to read, validate or audit an override never changes the last good
// gate or pauses the queue. Audit before changing admission, outside its lock;
// an overloaded writer must not block grants or status readers behind this IO.
func (scheduler *Scheduler) refreshRuntimeCapacity(ctx context.Context) {
	scheduler.capacityReloadMu.Lock()
	defer scheduler.capacityReloadMu.Unlock()
	if scheduler.capacityFile == "" && scheduler.capacityOverride == nil {
		return
	}
	jobs, source, err := scheduler.desiredRuntimeCapacity(ctx)
	gate := scheduler.admission
	gate.mu.Lock()
	previous := gate.capacityConfig
	budget, validationErr := jobs, error(nil)
	if !gate.workflowOnly && err == nil {
		budget, validationErr = admissionCapacityBudget(jobs, gate.weights, true)
	}
	gate.mu.Unlock()
	if err == nil {
		err = validationErr
	}
	if err != nil {
		scheduler.capacityReloadError(err)
		return
	}
	if previous.MaxConcurrentJobs == jobs && previous.Source == source {
		gate.mu.Lock()
		gate.capacityConfig.Error = ""
		gate.mu.Unlock()
		return
	}
	details, err := json.Marshal(map[string]any{"previous_jobs": previous.MaxConcurrentJobs, "previous_source": previous.Source, "jobs": jobs, "weight_budget": budget, "source": source, "file": scheduler.capacityFile})
	if err == nil {
		auditCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err = scheduler.requireMutation(auditCtx); err == nil {
			_, err = scheduler.auditor.AppendAuditAction(auditCtx, model.AuditActionSpec{Actor: "system", Action: "admission.capacity.change", ResourceType: "admission", ResourceID: "capacity", Details: string(details)})
		}
	}
	if err != nil {
		scheduler.capacityReloadError(fmt.Errorf("capacity change could not be audited: %w", err))
		return
	}
	gate.mu.Lock()
	gate.capacity = budget
	gate.maxRuns = budget
	gate.capacityConfig.MaxConcurrentJobs = jobs
	gate.capacityConfig.Source = source
	gate.capacityConfig.Error = ""
	gate.capacityConfig.ChangedAt = time.Now().UTC()
	// Granted/retained reservations keep the exact weight they hold. Waiting
	// weights must be repriced, including whole-budget XL and clamped L weights.
	for _, waiter := range gate.waiters {
		if !gate.workflowOnly && waiter.prepared && !gate.usesDeclarations(waiter) {
			waiter.weight = gate.weights.of(waiter.size, budget)
		}
	}
	gate.grantLocked()
	gate.mu.Unlock()
	scheduler.signalQueue()
	log.Printf("admission capacity: %d concurrent runs, source=%s", jobs, source)
}

func (scheduler *Scheduler) watchRuntimeCapacity(ctx context.Context, ticks <-chan time.Time) {
	if ticks == nil {
		ticker := time.NewTicker(runtimeCapacityInterval)
		defer ticker.Stop()
		ticks = ticker.C
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			scheduler.refreshRuntimeCapacity(ctx)
		}
	}
}
