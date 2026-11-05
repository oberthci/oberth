package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/measured"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

type resourceHistoryStub struct {
	calls   int
	filter  store.RunResourceFilter
	repoID  int64
	history store.RunResourceHistory
}

func (s *resourceHistoryStub) RunResources(_ context.Context, id int64, f store.RunResourceFilter) (store.RunResourceHistory, error) {
	s.calls++
	s.repoID = id
	s.filter = f
	return s.history, nil
}

func TestRunResourcesRequiresResourceOnlyCapability(t *testing.T) {
	for _, mode := range []string{"legacy", "explicit-false", "resource-only"} {
		t.Run(mode, func(t *testing.T) {
			f := newScopeFixture(t)
			reader := &resourceHistoryStub{history: completeAdviceHistory(f.visible.Name)}
			f.service.runResources = reader
			args := map[string]any{"repo": f.visible.Name}
			if mode != "legacy" {
				args["resource_only"] = mode == "resource-only"
			}
			got, err := f.call(scopedActor, "run_resources", args)
			if err != nil {
				t.Fatal(err)
			}
			report := got.(measured.Report)
			if len(report.Steps) != 1 {
				t.Fatalf("missing measurements: %+v", report)
			}
			step := report.Steps[0]
			if step.Attempts != 5 || step.Samples != 20 || *step.MemoryPeakBytes.Max != 512<<20 || *step.AverageCPUNanoCores.P95 != 500_000_000 || *step.EphemeralBytes.Max != 100<<20 || *step.DurationNS.Max != int64(10*time.Second) {
				t.Fatalf("capability changed observations: %+v", step)
			}
			if mode == "resource-only" {
				if step.Suggested == nil || !step.SizingAcceptance.Sufficient || step.Suggested.CPUMilli != 630 || step.Suggested.MemoryLimitBytes != 2<<30 || step.Suggested.EphemeralLimitBytes != 512<<20 || step.Suggested.ActiveDeadlineSeconds != 0 || !strings.Contains(step.DeadlineNoSuggestion, "finalization") {
					t.Fatalf("supported resource advice: %+v", step)
				}
				return
			}
			// Previous CLIs decode into an int64 deadline, then unconditionally
			// re-encode it and copy it into YAML whenever suggested is non-null.
			body, err := json.Marshal(report)
			if err != nil {
				t.Fatal(err)
			}
			var legacy struct {
				Steps []struct {
					Suggested *struct {
						ActiveDeadlineSeconds int64 `json:"activeDeadlineSeconds"`
					} `json:"suggested"`
					NoSuggestion []string `json:"no_suggestion"`
					Semantics    string   `json:"semantics"`
				} `json:"steps"`
			}
			if err := json.Unmarshal(body, &legacy); err != nil {
				t.Fatal(err)
			}
			if legacy.Steps[0].Suggested != nil {
				t.Fatalf("previous CLI would fabricate a zero deadline: %s", body)
			}
			if step.SizingAcceptance.Sufficient || !strings.Contains(strings.Join(legacy.Steps[0].NoSuggestion, " "), "resource_only") || !strings.Contains(report.Semantics, "finalization") {
				t.Fatalf("legacy client lacks explanation: %s", body)
			}
		})
	}
}
func TestRunResourcesAuthorizesBeforeHistoryAndDefaults(t *testing.T) {
	f := newScopeFixture(t)
	reader := &resourceHistoryStub{}
	f.service.runResources = reader
	for _, repo := range []string{hiddenRepoName, "repo-99"} {
		raw, _ := json.Marshal(map[string]any{"repo": repo})
		if _, err := f.service.CallTool(t.Context(), scopedActor, "run_resources", raw); err == nil {
			t.Fatal("hidden repo allowed")
		}
	}
	if reader.calls != 0 {
		t.Fatal("unauthorized read")
	}
	got, err := f.call(scopedActor, "run_resources", map[string]any{"repo": f.visible.Name})
	if err != nil {
		t.Fatal(err)
	}
	if report, ok := got.(measured.Report); !ok || !report.IncludePromotions {
		t.Fatalf("type %T", got)
	}
	if reader.filter.Runs != 30 || reader.filter.Ref != f.visible.DefaultBranch || !reader.filter.IncludePromotions || reader.filter.IncludeFailed || reader.repoID != f.visible.ID {
		t.Fatalf("defaults: %+v", reader)
	}
	if _, err := f.call(scopedActor, "run_resources", map[string]any{"repo": f.visible.Name, "ref": f.visible.DefaultBranch}); err != nil {
		t.Fatal(err)
	}
	if reader.filter.IncludePromotions {
		t.Fatal("explicit ref unexpectedly included promotions")
	}
}
func TestRunResourcesRejectsBoundsAndHiddenExactRun(t *testing.T) {
	f := newScopeFixture(t)
	hiddenID := ""
	for _, runs := range f.runs {
		if len(runs) > 0 && runs[0].RepoID != f.visible.ID {
			hiddenID = runs[0].ID
			break
		}
	}
	reader := &resourceHistoryStub{}
	f.service.runResources = reader
	for _, args := range []map[string]any{{"repo": f.visible.Name, "last": 101}, {"repo": f.visible.Name, "last": -1}, {"repo": f.visible.Name, "format": "xml"}, {"repo": f.visible.Name, "run": hiddenID}, {"repo": f.visible.Name, "run": "garbage"}} {
		if _, err := f.call(scopedActor, "run_resources", args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	if reader.calls != 0 {
		t.Fatal("invalid input reached history")
	}
}

func completeAdviceHistory(owner string) store.RunResourceHistory {
	reader := &resourceHistoryStub{}
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	ptr := func(n int64) *int64 { return &n }
	for i := range measured.MinimumSamples {
		reader.history.Observations = append(reader.history.Observations, measured.Observation{
			Template: "compile", Owner: owner,
			Usage:            model.StepResourceUsage{Version: 2, Source: "kubelet-cadvisor", RunID: fmt.Sprint(i), PodUID: fmt.Sprint(i), ContainerID: fmt.Sprint(i), ContainerName: "main", ContainerKind: "app", PodKind: "workflow", Stage: "build", TemplateName: "compile", Terminal: true, Samples: 4, ContainerStartedAt: at.Add(-10 * time.Second), ContainerFinishedAt: at, CPUTimeNS: ptr(5e9), CPUTimeAt: at, CPUPeakNanoCores: ptr(1e9), MemoryPeakBytes: ptr(512 << 20), EphemeralStoragePeakBytes: ptr(100 << 20)},
			MemoryLimitBytes: ptr(2 << 30), CPULimitNanoCores: ptr(0), EphemeralLimitBytes: ptr(512 << 20),
		})
		v := &reader.history.Observations[len(reader.history.Observations)-1].Usage
		v.PodStartedAt = v.ContainerStartedAt
		v.PodFinishedAt = at
		v.MemoryCoverage = model.MetricCoverage{Count: 4, FirstAt: v.ContainerStartedAt, LastAt: at}
		v.CPUCoverage = v.MemoryCoverage
		v.StorageCoverage = v.MemoryCoverage
		v.PodEphemeralLimitBytes = ptr(512 << 20)
		v.CPURequestNanoCores = ptr(0)
		v.MemoryRequestBytes = ptr(0)
		v.EphemeralRequestBytes = ptr(0)
		code := int32(0)
		v.ExitCode = &code
		v.Reason = "Completed"
	}
	return reader.history
}
