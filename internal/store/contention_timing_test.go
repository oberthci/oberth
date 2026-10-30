package store

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func timingBatchFixture(t testing.TB, tasks int) (*Store, []model.StepTiming) {
	t.Helper()
	ctx := t.Context()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "timings.db"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	u, err := s.CreateUpstream(ctx, model.UpstreamSpec{Name: "codeberg", Kind: "forgejo", BaseURL: "https://codeberg.org"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.CreateRepository(ctx, model.RepositorySpec{Name: "test", UpstreamID: u.ID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.EnqueueRun(ctx, testRunSpec(r.ID, "main", strings.Repeat("a", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetRunJobName(ctx, run.ID, "workflow-test"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	wf := model.WorkflowTiming{RunID: run.ID, Namespace: "pipelines", Name: "workflow-test", UID: "11111111-1111-4111-8111-111111111111", SpecIdentity: strings.Repeat("a", 64), CreatedAt: now}
	values := make([]model.StepTiming, tasks)
	for i := range values {
		name := fmt.Sprintf("task-%d", i)
		wf.Tasks = append(wf.Tasks, model.TimingTask{ID: name, Template: name, Readiness: "all"})
		values[i] = model.StepTiming{Version: 1, RunID: run.ID, Namespace: wf.Namespace, WorkflowName: wf.Name, WorkflowUID: wf.UID, NodeID: name, Burn: "test", Step: name, Ordinal: i, ObservedAt: now, Unavailable: "not-executed", ScriptTiming: "mixed", ImagePullTiming: "not-observed"}
	}
	if err := s.RecordWorkflowTiming(ctx, wf); err != nil {
		t.Fatal(err)
	}
	return s, values
}

func TestStepTimingBatchBoundAndRollback(t *testing.T) {
	s, values := timingBatchFixture(t, 1)
	base := values[0]
	batch := make([]model.StepTiming, model.MaxTimingTasks-1)
	for i := range batch {
		batch[i] = base
		batch[i].NodeID = fmt.Sprintf("node-%d", i)
	}
	if err := s.RecordStepTimings(t.Context(), batch); err != nil {
		t.Fatal(err)
	}
	last := base
	last.NodeID = "last"
	// Repeated rows must not consume the cached per-run count.
	if err := s.RecordStepTimings(t.Context(), []model.StepTiming{batch[0], last, last}); err != nil {
		t.Fatal(err)
	}
	overflow := base
	overflow.NodeID = "overflow"
	if err := s.RecordStepTimings(t.Context(), []model.StepTiming{last, overflow}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("overflow accepted: %v", err)
	}
	if _, err := s.db.ExecContext(t.Context(), `DELETE FROM step_timings WHERE run_id=?`, base.RunID); err != nil {
		t.Fatal(err)
	}
	wrong := last
	wrong.WorkflowUID = "22222222-2222-4222-8222-222222222222"
	if err := s.RecordStepTimings(t.Context(), []model.StepTiming{base, wrong}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("mixed Workflow accepted: %v", err)
	}
	var count int
	if err := s.reader().QueryRowContext(t.Context(), `SELECT count(*) FROM step_timings WHERE run_id=?`, base.RunID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial batch committed: count=%d, %v", count, err)
	}
}

func BenchmarkRecordStepTimingsBatch(b *testing.B) {
	for _, tasks := range []int{64, 256} {
		b.Run(fmt.Sprint(tasks), func(b *testing.B) {
			s, values := timingBatchFixture(b, tasks)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				if _, err := s.db.ExecContext(b.Context(), `DELETE FROM step_timings WHERE run_id=?`, values[0].RunID); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
				if err := s.RecordStepTimings(b.Context(), values); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
