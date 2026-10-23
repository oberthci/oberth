package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
)

func TestPromotionRetainsFailedTrustedPreflightEvidence(t *testing.T) {
	for _, infrastructure := range []bool{false, true} {
		t.Run(fmt.Sprint(infrastructure), func(t *testing.T) {
			ctx := context.Background()
			f := newControlFixture(t)
			contract := []byte("version: 2\nprofile: kvm-suite-amd64-v1\nsuite_sha256: " + strings.Repeat("a", 64) + "\nguest_sha256: " + strings.Repeat("b", 64) + "\nartifacts: [build/program]\n")
			f.scheduler.git = &policyTestGit{controlGit: f.git, candidate: contract, baseline: contract}
			result := JobResult{Status: model.RunFailed, Phase: "test", FailedBurn: "test", FailedStep: "unit", Error: "command exited 1", Steps: []model.StepResult{{Burn: "test", Step: "unit", Status: model.StepFailed, ExitCode: 1}}}
			if infrastructure {
				result.Phase, result.FailedBurn, result.FailedStep, result.Error = "infrastructure", "", "", "Evicted: node memory pressure"
				result.Steps[0].ExitCode = 137
			}
			f.scheduler.trusted = &protectedFixture{store: f.store, failure: &TrustedPreflightFailure{Result: result, Cause: errors.New(result.Error)}}
			sha, base := strings.Repeat("c", 40), strings.Repeat("d", 40)
			queued, promotion, err := f.store.EnqueuePromotionRun(ctx, model.RunSpec{RepoID: f.repo.ID, RefKind: model.RefBranch, Ref: "promotion/main/preflight", SHA: sha, TestedSHA: sha, BaseSHA: base, Actor: "agent@host", Trigger: "promotion"}, model.PromotionSpec{RepoID: f.repo.ID, SourceBranch: "feature/preflight", SourceSHA: sha, TargetRef: "main", PreviousSHA: base, ResultSHA: sha, Actor: "agent@host"})
			if err != nil {
				t.Fatal(err)
			}
			if err := f.scheduler.ProcessNext(ctx); err != nil {
				t.Fatal(err)
			}
			control := f.api(t)
			value, err := control.CallTool(ctx, api.Actor{Identity: "agent@host"}, "run_get", json.RawMessage(fmt.Sprintf(`{"id":%q}`, queued.ID)))
			if err != nil {
				t.Fatal(err)
			}
			detail := value.(RunDetailResponse)
			if detail.Run.Status != model.RunFailed || detail.Run.Phase != result.Phase || detail.Run.FailedStep != result.FailedStep || !strings.Contains(detail.Run.Error, result.Error) {
				t.Fatalf("failed preflight classification: %#v", detail.Run)
			}
			if len(detail.Steps) != 1 || detail.Steps[0].Step != "unit" || detail.Steps[0].ExitCode != result.Steps[0].ExitCode || detail.Steps[0].LogEnd <= detail.Steps[0].LogStart {
				t.Fatalf("failed preflight steps: %#v", detail.Steps)
			}
			value, err = control.CallTool(ctx, api.Actor{Identity: "agent@host"}, "run_logs", json.RawMessage(fmt.Sprintf(`{"id":%q,"burn":"test","step":"unit"}`, queued.ID)))
			if err != nil {
				t.Fatal(err)
			}
			if got := value.(LogResponse); !strings.Contains(got.Output, "trusted step diagnostic") {
				t.Fatalf("failed preflight log: %#v", got)
			}
			current, err := f.store.Promotion(ctx, promotion.ID)
			if err != nil || current.Status != model.PromotionFailed || len(f.jobs.createdCI) != 0 || len(f.git.promotions) != 0 {
				t.Fatalf("failed preflight delivered promotion: %#v %v", current, err)
			}
			evidence, err := f.store.KVMPreflight(ctx, queued.ID)
			if err != nil || !evidence.Cleaned || evidence.Receipt != nil || evidence.Refusal == nil {
				t.Fatalf("failure evidence changed cleanup/receipt: %#v %v", evidence, err)
			}
		})
	}
}
