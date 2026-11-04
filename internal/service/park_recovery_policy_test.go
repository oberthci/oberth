package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/vmrunner"
)

type recoveredParkPolicyGit struct {
	*policyTestGit
	policy string
}

func (g *recoveredParkPolicyGit) ReadDefaultBlob(ctx context.Context, repo, file string, limit int) (string, []byte, bool, error) {
	if file == promotionPolicyFile {
		return strings.Repeat("b", 40), []byte(g.policy), true, nil
	}
	return g.policyTestGit.ReadDefaultBlob(ctx, repo, file, limit)
}

func TestRecoveredParkPromotionRetainsProtectedTargetPolicy(t *testing.T) {
	for _, train := range []bool{false, true} {
		t.Run(fmt.Sprint(train), func(t *testing.T) {
			f, executor, seed := trustedPublicationFixture(t, false, true)
			if _, err := f.store.FinishRun(t.Context(), seed.ID, model.RunResult{Status: model.RunPassed}); err != nil {
				t.Fatal(err)
			}
			credential, err := f.store.CreateTokenCredential(t.Context(), model.TokenCredentialSpec{Name: seed.Actor, Digest: make([]byte, 32)})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.store.RegisterUplink(t.Context(), "admin", model.UplinkSpec{Fingerprint: "SHA256:park-recovery", Identity: seed.Actor, TokenCredentialID: credential.ID, Admin: true}); err != nil {
				t.Fatal(err)
			}
			if _, err = f.store.ActivateTokenCredential(t.Context(), credential.ID); err != nil {
				t.Fatal(err)
			}
			f.git.plan = gitcache.MergeCandidate{BaseSHA: strings.Repeat("b", 40), MergedSHA: seed.SHA}
			c := f.api(t)
			defer func() { _ = c.Shutdown(context.Background()) }()
			c.git = &parkTrainGit{parkDeliveryGit: &parkDeliveryGit{DeliveryGit: f.git, ref: "park/review"}, train: &trainControlGit{controlGit: f.git}}
			parked := strings.Repeat("a", 40)
			args := fmt.Sprintf(`{"repo":%q,"branch":"main","sha":%q}`, f.repo.Name, parked)
			if train {
				args = fmt.Sprintf(`{"repo":%q,"branch":"main","sources":[{"sha":%q}]}`, f.repo.Name, parked)
			}
			value, err := c.CallTool(t.Context(), api.Actor{Identity: seed.Actor, Admin: true}, "promote", json.RawMessage(args))
			if err != nil {
				t.Fatal(err)
			}
			original := requireToolPromotion(t, f, value)
			if !original.BranchCISkipped {
				t.Fatal("fixture did not admit a parked source", original)
			}
			run, err := f.store.ClaimNextRun(t.Context())
			if err != nil || run.ID != original.RunID {
				t.Fatal(run, err)
			}
			contract := executor.intent.Contract
			execute := func(run model.Run) {
				t.Helper()
				if err := executor.Execute(t.Context(), WorkflowRequest{Run: run, Repository: f.repo, UpstreamName: "codeberg", UpstreamOrg: "acme"}, vmrunner.Requirement{CandidateSHA: run.TestedSHA, BaselineSHA: strings.Repeat("b", 40), Contract: &contract}, io.Discard); err != nil {
					t.Fatal(err)
				}
			}
			execute(run)
			next, err := f.store.RecoverKVMPreflight(t.Context(), run.ID)
			if err != nil || next.ID == "" {
				t.Fatal("recovery did not produce an exact fresh attempt", next, err)
			}
			retry, err := f.store.PromotionByRun(t.Context(), next.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !retry.BranchCISkipped {
				t.Error("recovery discarded skipped-branch evidence")
			}
			if !strings.Contains(next.Reason, "promoted without branch CI") {
				t.Error("recovery discarded the existing reason line")
			}
			audit, err := f.store.LatestAuditActionByType(t.Context(), "promotion.admit", "promotion")
			if err != nil || !strings.Contains(audit.Details, `"branch_ci_skipped":"true"`) {
				t.Error("recovery audit discarded skipped-branch evidence", audit, err)
			}
			next, err = f.store.ClaimNextRun(t.Context())
			if err != nil || next.ID != retry.RunID {
				t.Fatal(next, err)
			}
			execute(next)
			raw := []byte("version: 2\nprofile: kvm-suite-amd64-v1\nsuite_sha256: " + strings.Repeat("a", 64) + "\nguest_sha256: " + strings.Repeat("b", 64) + "\nartifacts: [build/program]\n")
			f.scheduler.git = &recoveredParkPolicyGit{policyTestGit: &policyTestGit{controlGit: f.git, candidate: raw, baseline: raw}, policy: "require_branch_ci: [main]\n"}
			// Fresh positive suite evidence must not bypass a newly protected target.
			err = f.scheduler.requirePublicationTestPolicy(t.Context(), f.repo, model.Publication{PromotionID: retry.ID, RunID: next.ID, Ref: "main", ResultSHA: next.SHA})
			if err == nil || !strings.Contains(err.Error(), "requires branch CI") {
				t.Errorf("recovered park source bypassed target policy: %v", err)
			}
			if len(f.git.promotions) != 0 {
				t.Fatal("recovery published before the policy gate")
			}
		})
	}
}
