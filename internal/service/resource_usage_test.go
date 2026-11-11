package service

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/storetest"
)

type reportReader struct {
	repoValues []model.StepResourceUsage
	calls      []int64
	values     []model.StepResourceUsage
	stepCalls  int
}

func (r *reportReader) StepResourceUsage(context.Context, string) ([]model.StepResourceUsage, bool, error) {
	r.stepCalls++
	return r.values, false, nil
}
func (r *reportReader) RepositoryResourceUsage(_ context.Context, id int64, _ store.ResourceUsageFilter) ([]model.StepResourceUsage, bool, error) {
	r.calls = append(r.calls, id)
	if r.repoValues != nil {
		return r.repoValues, true, nil
	}
	return []model.StepResourceUsage{{Stage: "checks", Step: "unit", Samples: 0, Terminal: true}}, true, nil
}
func TestResourceReportScopesBeforeReadingStorage(t *testing.T) {
	f := newScopeFixture(t)
	reader := &reportReader{}
	f.service.resources = reader
	for _, repo := range []string{"codeberg/" + hiddenRepoName, hiddenRepoName, "codeberg/absent"} {
		raw, _ := json.Marshal(map[string]any{"repo": repo})
		value, err := f.service.CallTool(context.Background(), scopedActor, "repo_usage", raw)
		if err == nil {
			t.Fatalf("hidden repo accepted %#v", value)
		}
	}
	if len(reader.calls) != 0 {
		t.Fatal("out-of-scope storage touched")
	}
	value, err := f.service.CallTool(context.Background(), scopedActor, "repo_usage", json.RawMessage(`{"repo":"oberth"}`))
	if err != nil {
		t.Fatal(err)
	}
	report, ok := value.(model.ResourceUsageReport)
	if !ok || !report.Truncated || !report.ReportOnly || report.SizeClassSemantics == "" || report.PopulationCoverageKnown || len(report.Steps) != 1 || report.Steps[0].Unsampled != 1 || len(report.Steps[0].ByWorkflowSize) != 1 || report.Steps[0].ByWorkflowSize[0].WorkflowSize != "unknown" || len(reader.calls) != 1 || reader.calls[0] != f.visible.ID {
		t.Fatalf("report %#v calls %v", value, reader.calls)
	}
	raw, _ := json.Marshal(value)
	if strings.Contains(string(raw), hiddenRepoName) {
		t.Fatal("hidden name leaked")
	}
	for _, args := range []string{`{}`, `{"repo":"oberth","limit":5001}`, `{"repo":"oberth","since":"bad"}`, `{"repo":"oberth","limit":-1}`} {
		if _, err = f.service.CallTool(context.Background(), scopedActor, "repo_usage", json.RawMessage(args)); err == nil {
			t.Fatalf("invalid report %s", args)
		}
	}
}

func TestResourceSizeComparisonRunGetIsScopedAndReadOnly(t *testing.T) {
	f := newScopeFixture(t)
	run := f.runs["github"][0]
	step := model.StepResult{RunID: run.ID, Stage: "test", Step: "unit", Status: model.StepFailed, ExitCode: 1, DeclaredSize: "S"}
	if _, err := storetest.PutHistoricalStepResult(f.store, t.Context(), step); err != nil {
		t.Fatal(err)
	}
	peak := int64(3 << 30)
	reader := &reportReader{values: []model.StepResourceUsage{{RunID: run.ID, Stage: "test", Step: "unit", WorkflowSize: "XL", Samples: 1, MemoryPeakBytes: &peak, ContainerName: "main"}}}
	helper := reader.values[0]
	helper.ContainerName = "wait"
	large := int64(100 << 30)
	helper.MemoryPeakBytes = &large
	reader.values = append(reader.values, helper)
	f.service.resources = reader
	pressure := pressureTestGate()
	pressure.sample.IO.FullAvg60 = 72
	pressure.prepare(1, model.PriorityStandard, "", "M")
	observer := &Scheduler{admission: pressure.gate}
	f.service.admission = observer
	before, _ := json.Marshal(observer.AdmissionState())
	beforeSteps, err := f.store.StepResults(t.Context(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.call(scopedActor, "run_get", map[string]any{"id": f.runs["codeberg"][0].ID}); err == nil || reader.stepCalls != 0 {
		t.Fatal("hidden run reached resource storage", err)
	}
	value, err := f.call(scopedActor, "run_get", map[string]any{"id": run.ID, "summary": false})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(value)
	if !strings.Contains(string(body), "MeasuredSizeComparison") || !strings.Contains(string(body), `"workflow_size":"XL"`) || reader.stepCalls == 0 {
		t.Fatalf("comparison absent: %s", body)
	}
	var decoded struct {
		Steps []struct {
			MeasuredUsage          []model.StepResourceUsage
			MeasuredSizeComparison []model.ResourceSizeComparison
		}
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Steps) != 1 || len(decoded.Steps[0].MeasuredUsage) != 2 || len(decoded.Steps[0].MeasuredSizeComparison) != 1 || decoded.Steps[0].MeasuredSizeComparison[0].Observations != 1 || *decoded.Steps[0].MeasuredSizeComparison[0].MemoryPeakBytes.Max != peak {
		t.Fatalf("run_get helper entered main size comparison: %s", body)
	}
	after, _ := json.Marshal(observer.AdmissionState())
	afterSteps, err := f.store.StepResults(t.Context(), run.ID)
	if err != nil || string(before) != string(after) || !reflect.DeepEqual(beforeSteps, afterSteps) || *reader.values[0].MemoryPeakBytes != peak {
		t.Fatal("read-only report changed admission, authored class or telemetry", err)
	}
}

func TestResourceReportIncludesBoundedContainersPodsAndOverlap(t *testing.T) {
	f := newScopeFixture(t)
	at := time.Now().UTC()
	reader := &reportReader{repoValues: []model.StepResourceUsage{}}
	for i := 0; i < 129; i++ {
		peak := int64(i + 1)
		reader.repoValues = append(reader.repoValues, model.StepResourceUsage{RunID: "run", PodUID: fmt.Sprintf("pod-%d", i), Stage: "ci", Step: fmt.Sprintf("step-%d", i), ContainerName: "wait", ContainerKind: "app", PodKind: "workflow", Samples: 1, MemoryPeakBytes: &peak, PodMemoryPeakBytes: &peak, PodStartedAt: at, PodFinishedAt: at.Add(time.Second)})
	}
	f.service.resources = reader
	before, _ := json.Marshal(reader.repoValues)
	value, err := f.call(scopedActor, "repo_usage", map[string]any{"repo": "oberth"})
	if err != nil {
		t.Fatal(err)
	}
	r := value.(model.ResourceUsageReport)
	if !r.ReportOnly || !r.Truncated || !r.ContainersTruncated || !r.PodsTruncated || len(r.Containers) != 128 || len(r.Pods) != 128 || len(r.Steps) != 0 || r.Overlap.Pods != 129 || *r.Overlap.PerRunPeakPods.Max != 129 || r.PopulationCoverageKnown {
		t.Fatal("report bounds/scopes lost", r)
	}
	after, _ := json.Marshal(reader.repoValues)
	if string(before) != string(after) {
		t.Fatal("report mutated observations")
	}
}
