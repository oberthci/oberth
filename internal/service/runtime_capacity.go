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
	if gate.capacityConfig.Source == "file" && gate.capacityConfig.MaxConcurrentJobs > 0 {
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
		log.Printf("WARN: admission capacity: %v; retaining %d M jobs", err, current)
	}
}

// Failure to read, validate or audit an override never changes the last good
// gate or pauses the queue. Audit before changing admission, outside its lock;
// an overloaded writer must not block grants or status readers behind this IO.
func (scheduler *Scheduler) refreshRuntimeCapacity(ctx context.Context) {
	if scheduler.capacityFile == "" {
		return
	}
	scheduler.capacityReloadMu.Lock()
	defer scheduler.capacityReloadMu.Unlock()
	jobs, err := readRuntimeCapacity(scheduler.capacityFile)
	gate := scheduler.admission
	gate.mu.Lock()
	previous := gate.capacityConfig
	budget, validationErr := admissionCapacityBudget(jobs, gate.weights, true)
	gate.mu.Unlock()
	if err == nil {
		err = validationErr
	}
	if err != nil {
		scheduler.capacityReloadError(err)
		return
	}
	if previous.MaxConcurrentJobs == jobs && previous.Source == "file" {
		gate.mu.Lock()
		gate.capacityConfig.Error = ""
		gate.mu.Unlock()
		return
	}
	details, err := json.Marshal(map[string]any{"previous_jobs": previous.MaxConcurrentJobs, "previous_source": previous.Source, "jobs": jobs, "weight_budget": budget, "source": "file", "file": scheduler.capacityFile})
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
	gate.capacityConfig.Source = "file"
	gate.capacityConfig.Error = ""
	gate.capacityConfig.ChangedAt = time.Now().UTC()
	// Granted/retained reservations keep the exact weight they hold. Waiting
	// weights must be repriced, including whole-budget XL and clamped L weights.
	for _, waiter := range gate.waiters {
		if waiter.prepared {
			waiter.weight = gate.weights.of(waiter.size, budget)
		}
	}
	gate.grantLocked()
	gate.mu.Unlock()
	scheduler.signalQueue()
	log.Printf("admission capacity: %d M jobs, weight budget %d, source=file", jobs, budget)
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
