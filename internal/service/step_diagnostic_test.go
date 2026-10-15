package service

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
)

func TestRunGetRetainsBoundTerminationAndUnavailableHistory(t *testing.T) {
	f := newControlFixture(t)
	ctx := context.Background()
	run, err := f.scheduler.EnqueueCI(ctx, CIRequest{EventID: "termination-read", Repository: f.repo, Branch: "feature/termination", SHA: strings.Repeat("a", 40), Actor: "reader@host"})
	if err != nil {
		t.Fatal(err)
	}
	observed := time.Now().UTC()
	record := &model.StepDiagnostic{Available: true, ObservedAt: &observed, Container: &model.StepTermination{RunID: run.ID, Namespace: "pipelines", WorkflowName: "workflow-test", WorkflowUID: "11111111-1111-4111-8111-111111111111", NodeID: "workflow-test-1234", PodName: "workflow-test-main-1234", PodUID: "22222222-2222-4222-8222-222222222222", ContainerName: "main", ContainerID: "containerd://" + strings.Repeat("c", 64), Reason: "OOMKilled", PodReason: "none", ExitCode: 137, StartedAt: observed.Add(-time.Minute), FinishedAt: observed.Add(-time.Second)}}
	for _, name := range []string{"new", "legacy"} {
		step := model.StepResult{RunID: run.ID, Burn: "test", Step: name, Status: model.StepFailed, ExitCode: 137, DeclaredSize: "M"}
		if name == "new" {
			step.Termination = record
		}
		if _, err = f.store.PutStepResult(ctx, step); err != nil {
			t.Fatal(err)
		}
	}
	before, err := f.store.VerifyAuditChain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	value, err := f.api(t).CallTool(ctx, api.Actor{Identity: "reader@host"}, "run_get", json.RawMessage(fmt.Sprintf(`{"id":%q}`, run.ID)))
	if err != nil {
		t.Fatal(err)
	}
	detail := value.(RunDetailResponse)
	if detail.Run.ID != run.ID || detail.Repository.ID != f.repo.ID || len(detail.Steps) != 2 {
		t.Fatal("run binding lost")
	}
	// This qualifies the native tool DTO and JSON encoding, not an actual
	// network MCP SDK transport or a mutable Kubernetes lookup.
	raw, err := json.Marshal(detail)
	if err != nil {
		t.Fatal(err)
	}
	var decoded RunDetailResponse
	if json.Unmarshal(raw, &decoded) != nil {
		t.Fatal("wire decode failed")
	}
	for _, s := range decoded.Steps {
		if s.Status != model.StepFailed || s.ExitCode != 137 || s.Termination == nil {
			t.Fatal("verdict or observation lost")
		}
		if s.Step == "new" {
			if !s.Termination.Available || s.Termination.Container.RunID != run.ID || s.Termination.Container.Reason != "OOMKilled" {
				t.Fatal("bound reason lost")
			}
		} else if s.Termination.Unavailable != "not-recorded" {
			t.Fatal("invented legacy evidence")
		}
	}
	after, err := f.store.VerifyAuditChain(ctx)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("read mutated audit")
	}
}
