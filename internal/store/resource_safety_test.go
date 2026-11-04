package store

import (
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/measured"
	"github.com/oberthci/oberth/internal/model"
)

func coveredResourceRow(runID string, at time.Time) model.StepResourceUsage {
	v := resourceRow(runID, at)
	ptr := func(n int64) *int64 { return &n }
	v.Version = 2
	v.ContainerKind = "app"
	v.PodKind = "workflow"
	v.Stage = "build"
	v.Step = "compile"
	v.TemplateName = "compile"
	v.FragmentRepository = "repo-01"
	v.Terminal = true
	v.Reason = "Completed"
	code := int32(0)
	v.ExitCode = &code
	v.Samples = 4
	v.ContainerStartedAt = at.Add(-10 * time.Second)
	v.ContainerFinishedAt = at
	v.PodStartedAt = v.ContainerStartedAt
	v.PodFinishedAt = at
	c := model.MetricCoverage{Count: 4, FirstAt: v.ContainerStartedAt, LastAt: at}
	v.MemoryCoverage = c
	v.CPUCoverage = c
	v.StorageCoverage = c
	v.CPUTimeNS = ptr(5e9)
	v.CPUTimeAt = at
	v.CPUPeakNanoCores = ptr(1e9)
	v.MemoryPeakBytes = ptr(256 << 20)
	v.EphemeralStoragePeakBytes = ptr(100 << 20)
	v.CPULimitNanoCores = ptr(2e9)
	v.MemoryLimitBytes = ptr(1 << 30)
	v.EphemeralLimitBytes = ptr(512 << 20)
	v.PodEphemeralLimitBytes = ptr(512 << 20)
	v.CPURequestNanoCores = ptr(0)
	v.MemoryRequestBytes = ptr(0)
	v.EphemeralRequestBytes = ptr(0)
	return v
}

func TestResourceSafetyIgnoresFailedDisplayFilterAndCatchesZeroRows(t *testing.T) {
	for _, kind := range []string{"success", "skipped-unexecuted", "evicted", "failed-without-usage", "unrelated-ref", "unrelated-template", "unrelated-burn"} {
		t.Run(kind, func(t *testing.T) {
			s, base, at := resourceStoreFixture(t)
			repo, e := s.Repository(t.Context(), base.RepoID)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = s.CreateRepository(t.Context(), model.RepositorySpec{Name: "other-resource", UpstreamID: repo.UpstreamID, DefaultBranch: "main"}); e != nil {
				t.Fatal(e)
			}
			var first model.Run
			for i := range 5 {
				r, e := s.EnqueueRun(t.Context(), testRunSpec(base.RepoID, "main", strings.Repeat(string(rune('a'+i)), 40)))
				if e != nil {
					t.Fatal(e)
				}
				first = r.Run
				if _, e = s.SetRunWorkflowName(t.Context(), r.ID, "workflow-test"); e != nil {
					t.Fatal(e)
				}
				if e = s.RecordStepResourceUsage(t.Context(), []model.StepResourceUsage{coveredResourceRow(r.ID, at)}); e != nil {
					t.Fatal(e)
				}
				if _, e = s.db.Exec(`UPDATE runs SET status='passed' WHERE id=?`, r.ID); e != nil {
					t.Fatal(e)
				}
			}
			if kind != "success" {
				ref := "main"
				if kind == "unrelated-ref" {
					ref = "other"
				}
				r, e := s.EnqueueRun(t.Context(), testRunSpec(base.RepoID, ref, strings.Repeat("f", 40)))
				if e != nil {
					t.Fatal(e)
				}
				if _, e = s.SetRunWorkflowName(t.Context(), r.ID, "workflow-test"); e != nil {
					t.Fatal(e)
				}
				if kind == "failed-without-usage" || kind == "skipped-unexecuted" {
					stepStatus := model.StepFailed
					if kind == "skipped-unexecuted" {
						stepStatus = model.StepSkipped
					}
					if _, e = s.PutStepResult(t.Context(), model.StepResult{RunID: r.ID, Stage: "build", Step: "compile", Status: stepStatus, ExitCode: 1, DeclaredSize: "M"}); e != nil {
						t.Fatal(e)
					}
				} else {
					template := "compile"
					if kind == "unrelated-template" {
						template = "other"
					}
					stage := "build"
					if kind == "unrelated-burn" {
						stage = "release-build"
					}
					f := model.PodResourceDisposition{RunID: r.ID, WorkflowName: "workflow-test", WorkflowUID: "11111111-1111-4111-8111-111111111111", Namespace: "argo", NodeID: "wf-1", PodName: "wf-1", PodUID: "22222222-2222-4222-8222-222222222222", Stage: stage, Step: template, Template: template, Owner: "repo-01", Reason: "Evicted", ObservedAt: at}
					if e = s.RecordPodResourceDisposition(t.Context(), f); e != nil {
						t.Fatal(e)
					}
				}
				if _, e = s.db.Exec(`UPDATE runs SET status='failed' WHERE id=?`, r.ID); e != nil {
					t.Fatal(e)
				}
			}
			for _, include := range []bool{false, true} {
				h, e := s.RunResources(t.Context(), base.RepoID, RunResourceFilter{Ref: "main", Runs: 30, IncludeFailed: include})
				if e != nil {
					t.Fatal(e)
				}
				steps := measured.BuildWithSafety(h.Observations, nil, h.SafetyObservations, h.Dispositions)
				var advice *measured.Suggested
				for _, step := range steps {
					if step.Template == "compile" && step.Stage == "build" {
						advice = step.Suggested
					}
				}
				unsafe := kind == "evicted" || kind == "failed-without-usage"
				if unsafe {
					if advice != nil && !h.SafetyIncomplete {
						t.Fatalf("include_failed=%t hid %s failure", include, kind)
					}
				} else if advice == nil || h.SafetyIncomplete {
					t.Fatalf("positive/unrelated control refused (%s): %+v", kind, h)
				}
				other, e := s.RunResources(t.Context(), base.RepoID+1, RunResourceFilter{RunID: first.ID, Runs: 1, IncludeFailed: include})
				if e != nil || len(other.Dispositions) > 0 || len(other.SafetyObservations) > 0 || len(other.SafetyRuns) > 0 || other.SafetyIncomplete {
					t.Fatal("repository safety evidence leaked", e)
				}
			}
		})
	}
}
