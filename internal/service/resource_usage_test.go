package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

type reportReader struct{ calls []int64 }

func (*reportReader) StepResourceUsage(context.Context, string) ([]model.StepResourceUsage, bool, error) {
	return nil, false, nil
}
func (r *reportReader) RepositoryResourceUsage(_ context.Context, id int64, _ store.ResourceUsageFilter) ([]model.StepResourceUsage, bool, error) {
	r.calls = append(r.calls, id)
	return []model.StepResourceUsage{{Burn: "checks", Step: "unit", Samples: 0, Terminal: true}}, true, nil
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
	if !ok || !report.Truncated || report.PopulationCoverageKnown || len(report.Steps) != 1 || report.Steps[0].Unsampled != 1 || len(reader.calls) != 1 || reader.calls[0] != f.visible.ID {
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
