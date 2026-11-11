package service

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/measured"
	"github.com/oberthci/oberth/internal/model"
)

func TestDefaultResourceAdviceUsesFailuresWithoutMetricRows(t *testing.T) {
	for _, kind := range []string{"success", "skipped-unexecuted", "evicted-no-usage", "step-no-usage", "oom-with-usage", "zero-samples-with-usage", "partial-with-usage", "unrelated-ref", "unrelated-template"} {
		t.Run(kind, func(t *testing.T) {
			f := newScopeFixture(t)
			repo, err := f.store.CreateRepository(t.Context(), model.RepositorySpec{Name: "resource-control", UpstreamID: f.github.ID, DefaultBranch: "main"})
			if err != nil {
				t.Fatal(err)
			}
			f.service.runResources = f.store
			base := completeAdviceHistory(repo.Name)
			at := time.Now().UTC()
			makeRun := func(ref string, i int, status model.RunStatus) model.Run {
				r, e := f.store.EnqueueRun(t.Context(), model.RunSpec{RepoID: repo.ID, RefKind: model.RefBranch, Ref: ref, SHA: fmt.Sprintf("%040x", i+1), Actor: "fixture", Trigger: "push"})
				if e != nil {
					t.Fatal(e)
				}
				if _, e = f.store.SetRunWorkflowName(t.Context(), r.ID, "workflow-test"); e != nil {
					t.Fatal(e)
				}
				if i < 5 || strings.HasSuffix(kind, "with-usage") {
					v := base.Observations[i%5].Usage
					v.Step = "compile"
					v.RunID = r.ID
					v.WorkflowName = "workflow-test"
					v.WorkflowUID = "11111111-1111-4111-8111-111111111111"
					v.Namespace = "argo"
					v.NodeID = "wf-1"
					v.PodName = "wf-1"
					v.PodUID = "22222222-2222-4222-8222-222222222222"
					v.NodeName = "node-a"
					v.ContainerID = "containerd://" + strings.Repeat("b", 64)
					v.FirstObservedAt = v.ContainerStartedAt
					v.LastObservedAt = v.ContainerFinishedAt
					v.SourceAt = v.ContainerFinishedAt
					v.SampledLowerBound = true
					v.CPULimitNanoCores = base.Observations[i%5].CPULimitNanoCores
					v.MemoryLimitBytes = base.Observations[i%5].MemoryLimitBytes
					v.EphemeralLimitBytes = base.Observations[i%5].EphemeralLimitBytes
					if i >= 5 {
						code := int32(137)
						v.Reason, v.ExitCode = "OOMKilled", &code
						switch kind {
						case "zero-samples-with-usage":
							v.Samples, v.SourceAt = 0, time.Time{}
							v.MemoryCoverage, v.CPUCoverage, v.StorageCoverage = model.MetricCoverage{}, model.MetricCoverage{}, model.MetricCoverage{}
							v.MemoryPeakBytes, v.CPUTimeNS, v.CPUPeakNanoCores, v.EphemeralStoragePeakBytes = nil, nil, nil, nil
							v.CPUTimeAt = time.Time{}
						case "partial-with-usage":
							v.Terminal, v.Reason, v.ExitCode = false, "", nil
							v.ContainerFinishedAt, v.PodFinishedAt = time.Time{}, time.Time{}
						}
					}
					attemptStatus := model.StepRunning
					if v.Terminal {
						attemptStatus = model.StepPassed
					}
					if v.Terminal && (v.Reason != "Completed" || (v.ExitCode != nil && *v.ExitCode != 0)) {
						attemptStatus = model.StepFailed
					}
					if e = f.store.RecordResourceAttempts(t.Context(), model.ResourceAttemptInventory{RunID: v.RunID, WorkflowName: v.WorkflowName, WorkflowUID: v.WorkflowUID, Namespace: v.Namespace, Complete: true, Terminal: i < 5 || !strings.HasPrefix(kind, "active-"), Attempts: []model.ResourceAttempt{{NodeID: v.NodeID, Stage: v.Stage, Step: v.Step, Template: v.TemplateName, Status: attemptStatus, PodUID: v.PodUID}}}); e != nil {
						t.Fatal(e)
					}
					if e = f.store.RecordStepResourceUsage(t.Context(), []model.StepResourceUsage{v}); e != nil {
						t.Fatal(e)
					}
				}
				if i >= 5 {
					return r.Run
				}
				result, e := f.store.FinishRun(t.Context(), r.ID, model.RunResult{Status: status})
				if e != nil {
					t.Fatal(e)
				}
				return result
			}
			for i := range 5 {
				makeRun("main", i, model.RunPassed)
			}
			if kind != "success" {
				ref := "main"
				if kind == "unrelated-ref" {
					ref = "other"
				}
				r := makeRun(ref, 5, model.RunFailed)
				if kind == "step-no-usage" || kind == "skipped-unexecuted" {
					stepStatus := model.StepFailed
					if kind == "skipped-unexecuted" {
						stepStatus = model.StepSkipped
					}
					if _, err = f.store.PutStepResult(t.Context(), model.StepResult{RunID: r.ID, Stage: "build", Step: "compile", Status: stepStatus, ExitCode: 1, DeclaredSize: "M"}); err != nil {
						t.Fatal(err)
					}
				} else if !strings.HasSuffix(kind, "with-usage") {
					template := "compile"
					if kind == "unrelated-template" {
						template = "other"
					}
					d := model.PodResourceDisposition{RunID: r.ID, WorkflowName: "workflow-test", WorkflowUID: "11111111-1111-4111-8111-111111111111", Namespace: "argo", NodeID: "wf-1", PodName: "wf-1", PodUID: "22222222-2222-4222-8222-222222222222", Stage: "build", Step: template, Template: template, Owner: repo.Name, Reason: "Evicted", ObservedAt: at}
					attemptStatus := model.StepFailed
					if d.Reason == "Completed" {
						attemptStatus = model.StepPassed
					}
					if err = f.store.RecordResourceAttempts(t.Context(), model.ResourceAttemptInventory{RunID: d.RunID, WorkflowName: d.WorkflowName, WorkflowUID: d.WorkflowUID, Namespace: d.Namespace, Complete: true, Terminal: !strings.HasPrefix(kind, "active-"), Attempts: []model.ResourceAttempt{{NodeID: d.NodeID, Stage: d.Stage, Step: d.Step, Template: d.Template, Status: attemptStatus, PodUID: d.PodUID}}}); err != nil {
						t.Fatal(err)
					}
					if err = f.store.RecordPodResourceDisposition(t.Context(), d); err != nil {
						t.Fatal(err)
					}
				}
				if _, err = f.store.FinishRun(t.Context(), r.ID, model.RunResult{Status: model.RunFailed}); err != nil {
					t.Fatal(err)
				}
			}
			for _, mode := range []string{"omitted", "false", "true"} {
				args := map[string]any{"repo": repo.Name}
				if mode != "omitted" {
					args["include_failed"] = mode == "true"
				}
				value, err := f.call(scopedActor, "run_resources", args)
				if err != nil {
					t.Fatal(err)
				}
				report := value.(measured.Report)
				var found *measured.Step
				for i := range report.Steps {
					if report.Steps[i].Template == "compile" {
						found = &report.Steps[i]
					}
				}
				wantMemory, wantRuns := 5, 5
				if mode == "true" && kind == "oom-with-usage" {
					wantMemory, wantRuns = 6, 6
				}
				if mode == "true" && kind == "zero-samples-with-usage" {
					wantRuns = 6
				}
				if found == nil || found.MemoryPeakBytes.Count != wantMemory || found.Runs != wantRuns {
					t.Fatalf("display measurements disappeared: %+v", report)
				}
				unsafe := kind == "evicted-no-usage" || kind == "step-no-usage" || strings.HasSuffix(kind, "with-usage")
				if (found.Suggested == nil) != unsafe {
					t.Fatalf("include_failed=%s kind=%s advice=%+v reasons=%v", mode, kind, found.Suggested, found.NoSuggestion)
				}
				if unsafe && len(found.NoSuggestion) == 0 {
					t.Fatal("refusal lacks explanation")
				}
			}
		})
	}
}
