package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func resourceRow(runID string, at time.Time) model.StepResourceUsage {
	n := int64(123)
	return model.StepResourceUsage{Version: 1, Source: "kubelet-cadvisor", RunID: runID, WorkflowName: "workflow-test", WorkflowUID: "11111111-1111-4111-8111-111111111111", Namespace: "argo", NodeID: "workflow-test-123", PodName: "workflow-test-123", PodUID: "22222222-2222-4222-8222-222222222222", NodeName: "node-a", ContainerID: "containerd://" + strings.Repeat("b", 64), ContainerName: "main", Burn: "checks", Step: "test", FirstObservedAt: at, LastObservedAt: at, SourceAt: at, Samples: 1, SampledLowerBound: true, MemoryPeakBytes: &n, IOReadBytes: &n}
}
func TestResourceUsageDurableConcurrentKilledAndRepositoryIsolation(t *testing.T) {
	at := time.Now().UTC()
	s := testStore(t, &at)
	ctx := context.Background()
	repo := createRepo(t, s)
	run, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/resource", strings.Repeat("a", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SetRunJobName(ctx, run.ID, "workflow-test"); err != nil {
		t.Fatal(err)
	}
	v := resourceRow(run.ID, at)
	if err = s.RecordStepResourceUsage(ctx, []model.StepResourceUsage{v}); err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	for range 8 {
		wait.Go(func() {
			if err := s.RecordStepResourceUsage(ctx, []model.StepResourceUsage{v}); err != nil {
				t.Error(err)
			}
		})
	}
	wait.Wait()
	killed := v
	killed.SourceAt = time.Time{}
	killed.Samples = 0
	killed.MemoryPeakBytes = nil
	killed.IOReadBytes = nil
	killed.Terminal = true
	code := int32(137)
	killed.ExitCode = &code
	killed.Reason = "OOMKilled"
	killed.LastObservedAt = at.Add(time.Second)
	if err = s.RecordStepResourceUsage(ctx, []model.StepResourceUsage{killed}); err != nil {
		t.Fatal(err)
	}
	rows, more, err := s.StepResourceUsage(ctx, run.ID)
	if err != nil || more || len(rows) != 1 || !rows[0].Terminal || rows[0].Samples != 1 || *rows[0].MemoryPeakBytes != 123 || *rows[0].IOReadBytes != 123 {
		t.Fatalf("durable killed %#v %v", rows, err)
	}
	for _, change := range []string{"job", "uid", "invalid"} {
		bad := v
		switch change {
		case "job":
			bad.WorkflowName = "foreign"
		case "uid":
			bad.WorkflowUID = "33333333-3333-4333-8333-333333333333"
		case "invalid":
			bad.MemoryPeakBytes = new(int64)
			*bad.MemoryPeakBytes = -1
		}
		if err = s.RecordStepResourceUsage(ctx, []model.StepResourceUsage{bad}); err == nil {
			t.Fatalf("accepted %s", change)
		}
	}
	rows, more, err = s.RepositoryResourceUsage(ctx, repo.ID+100, ResourceUsageFilter{})
	if err != nil || more || len(rows) != 0 {
		t.Fatal("cross-repository usage leak")
	}
	// Separate incarnation, never merged into the killed attempt.
	v.ContainerID = "containerd://" + strings.Repeat("c", 64)
	v.RestartCount = 1
	v.SourceAt = at.Add(2 * time.Second)
	v.LastObservedAt = v.SourceAt
	if err = s.RecordStepResourceUsage(ctx, []model.StepResourceUsage{v}); err != nil {
		t.Fatal(err)
	}
	rows, more, err = s.RepositoryResourceUsage(ctx, repo.ID, ResourceUsageFilter{Limit: 1})
	if err != nil || !more || len(rows) != 1 || rows[0].RestartCount != 1 {
		t.Fatalf("bounded report %#v %v %v", rows, more, err)
	}
	if _, _, err = s.RepositoryResourceUsage(ctx, repo.ID, ResourceUsageFilter{Limit: 5001}); !errors.Is(err, ErrInvalid) {
		t.Fatal("report limit not enforced")
	}
	var plan string
	queryRows, err := s.db.Query(`EXPLAIN QUERY PLAN SELECT observation_json FROM step_resource_usage WHERE repo_id=? AND last_observed_at>=? ORDER BY last_observed_at DESC LIMIT ?`, repo.ID, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer queryRows.Close()
	for queryRows.Next() {
		var a, b, c int
		if err = queryRows.Scan(&a, &b, &c, &plan); err != nil {
			t.Fatal(err)
		}
	}
	if !strings.Contains(plan, "step_resource_usage_repo_idx") {
		t.Fatalf("report unindexed: %s", plan)
	}
}

func TestResourceUsageMigrationPreservesSchema35AndExistingRows(t *testing.T) {
	at := time.Now()
	path := filepath.Join(t.TempDir(), "usage.db")
	s, err := Open(context.Background(), path, Options{Now: func() time.Time { return at }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	repo := createRepo(t, s)
	if _, err := s.db.Exec(`DROP TABLE step_resource_usage;DELETE FROM schema_migrations WHERE version=36`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE schema_migrations SET applied_at=123 WHERE version=35`); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path, Options{Now: func() time.Time { return at }})
	if err != nil {
		t.Fatal(err)
	}
	var old int64
	if err := s.db.QueryRow(`SELECT applied_at FROM schema_migrations WHERE version=35`).Scan(&old); err != nil || old != 123 {
		t.Fatalf("schema35 rewritten: %d %v", old, err)
	}
	got, err := s.Repository(ctx, repo.ID)
	if err != nil || got.Name != repo.Name {
		t.Fatal("existing repository lost")
	}
	var name string
	if err := s.db.QueryRow(`SELECT name FROM sqlite_master WHERE type='index' AND name='runs_latest_ref_idx'`).Scan(&name); err != nil {
		t.Fatal(fmt.Errorf("RCA index lost: %w", err))
	}
}
