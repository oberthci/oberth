package service

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/vmrunner"
)

type shutdownPreflight struct {
	*protectedFixture
	started chan struct{}
}

func (p *shutdownPreflight) Execute(ctx context.Context, job JobRequest, requirement vmrunner.Requirement, output io.Writer) error {
	// Persist a submitted, bound VM without results or cleanup, then lose the
	// process's observer. The new owner must not infer a positive result.
	if err := p.protectedFixture.Execute(ctx, job, requirement, output); err == nil {
		return errors.New("fixture unexpectedly completed")
	}
	close(p.started)
	<-ctx.Done()
	return ctx.Err()
}

type recoveringPreflight struct{ *protectedFixture }

func (p *recoveringPreflight) Recover(ctx context.Context) ([]string, error) {
	pending, err := p.store.PendingKVMPreflights(ctx)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for _, state := range pending {
		if !state.Cleaned {
			if state.Refusal == nil && state.Receipt == nil {
				if err = p.store.RecordKVMRefusal(ctx, state.Intent.RunID, vmrunner.KVMRefusal{Code: "canceled", ObservedAt: time.Now().UTC()}); err != nil {
					return nil, err
				}
			}
			if err = p.store.CompleteKVMPreflightCleanup(ctx, state.Intent.RunID, state.WorkflowUID); err != nil {
				return nil, err
			}
		}
		ids = append(ids, state.Intent.RunID)
	}
	return ids, nil
}

func TestTrustedRestartInterruptsThenRunsFreshPreflightBeforeDelivery(t *testing.T) {
	fixture := newControlFixture(t, JobResult{Status: model.RunPassed, Phase: "Succeeded"})
	raw := []byte("version: 2\nprofile: kvm-suite-amd64-v1\nsuite_sha256: " + strings.Repeat("a", 64) + "\nguest_sha256: " + strings.Repeat("b", 64) + "\nartifacts: [build/program]\n")
	fixture.scheduler.git = &policyTestGit{controlGit: fixture.git, candidate: raw, baseline: raw}
	first := &shutdownPreflight{protectedFixture: &protectedFixture{store: fixture.store, omitReceipt: true, omitCleanup: true}, started: make(chan struct{})}
	fixture.scheduler.trusted = first
	queued, err := fixture.scheduler.EnqueueCI(t.Context(), CIRequest{EventID: "restart-preflight", Repository: fixture.repo, Branch: "feature/restart", SHA: strings.Repeat("c", 40), Actor: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- fixture.scheduler.ProcessNext(ctx) }()
	select {
	case <-first.started:
	case <-time.After(time.Second):
		t.Fatal("preflight never started")
	}
	cancel()
	if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	source, err := fixture.store.Run(t.Context(), queued.ID)
	if err != nil || source.Status != model.RunRunning || source.JobName != "" {
		t.Fatalf("shutdown lost recovery obligation: %+v %v", source, err)
	}
	if len(fixture.jobs.createdCI) != 0 || len(fixture.git.syncedBranches) != 0 {
		t.Fatal("shutdown delivered without trusted evidence")
	}
	if err := fixture.store.RecoverOwnerState(t.Context()); err != nil {
		t.Fatal(err)
	}
	executor := &recoveringPreflight{protectedFixture: &protectedFixture{store: fixture.store}}
	scheduler, err := NewScheduler(SchedulerConfig{Store: fixture.store, Git: fixture.scheduler.git, Logs: fixture.scheduler.logs, Jobs: fixture.jobs, ReleaseJobs: fixture.jobs, Auditor: fixture.store, TrustedPreflight: executor, WorkspaceRoot: fixture.scheduler.workspaceRoot})
	if err != nil {
		t.Fatal(err)
	}
	if err = scheduler.recoverTrustedPreflights(t.Context()); err != nil {
		t.Fatal(err)
	}
	source, err = fixture.store.Run(t.Context(), source.ID)
	if err != nil || source.Status != model.RunInterrupted || source.SupersededBy == "" {
		t.Fatalf("restart not settled: %+v %v", source, err)
	}
	old, err := fixture.store.KVMPreflight(t.Context(), source.ID)
	if err != nil || old.Refusal == nil || old.Receipt != nil || !old.Cleaned {
		t.Fatalf("negative cleanup receipt missing: %+v %v", old, err)
	}
	if _, err = fixture.store.KVMPreflight(t.Context(), source.SupersededBy); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("old receipt applied to retry")
	}
	if err = scheduler.ProcessNext(t.Context()); err != nil {
		t.Fatal(err)
	}
	next, err := fixture.store.Run(t.Context(), source.SupersededBy)
	if err != nil || next.Status != model.RunPassed || next.AdmissionSequence != source.AdmissionSequence {
		t.Fatalf("retry did not complete: %+v %v", next, err)
	}
	evidence, err := fixture.store.KVMPreflight(t.Context(), next.ID)
	if err != nil || evidence.Receipt == nil || evidence.Refusal != nil || !evidence.Cleaned || evidence.Intent.RunID == old.Intent.RunID {
		t.Fatalf("fresh positive evidence missing: %+v %v", evidence, err)
	}
	if len(fixture.jobs.createdCI) != 1 || len(fixture.git.syncedBranches) != 1 {
		t.Fatalf("unexpected delivery count: jobs=%d pushes=%d", len(fixture.jobs.createdCI), len(fixture.git.syncedBranches))
	}
}

func TestTrustedRestartPromotionStillChecksMainBeforePush(t *testing.T) {
	for _, moved := range []bool{false, true} {
		t.Run(map[bool]string{false: "same main", true: "main moved"}[moved], func(t *testing.T) {
			f, executor, source := trustedPublicationFixture(t, false, true)
			if _, err := f.store.FinishRun(t.Context(), source.ID, model.RunResult{Status: model.RunPassed}); err != nil {
				t.Fatal(err)
			}
			credential, err := f.store.CreateTokenCredential(t.Context(), model.TokenCredentialSpec{Name: source.Actor, Digest: make([]byte, 32)})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.store.RegisterUplink(t.Context(), "admin", model.UplinkSpec{Fingerprint: "SHA256:restart-promoter", Identity: source.Actor, TokenCredentialID: credential.ID, Admin: true}); err != nil {
				t.Fatal(err)
			}
			if _, err = f.store.ActivateTokenCredential(t.Context(), credential.ID); err != nil {
				t.Fatal(err)
			}
			base := strings.Repeat("b", 40)
			_, promotion, err := f.store.EnqueuePromotionRun(t.Context(), model.RunSpec{RepoID: f.repo.ID, RefKind: model.RefBranch, Ref: "promotion/main/restart", SHA: source.SHA, TestedSHA: source.SHA, BaseSHA: base, Actor: source.Actor, Trigger: "promotion"}, model.PromotionSpec{RepoID: f.repo.ID, SourceBranch: source.Ref, SourceSHA: source.SHA, TargetRef: "main", PreviousSHA: base, ResultSHA: source.SHA, Actor: source.Actor})
			if err != nil {
				t.Fatal(err)
			}
			original, err := f.store.ClaimNextRun(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			contract := executor.intent.Contract
			if err = executor.Execute(t.Context(), JobRequest{Run: original, Repository: f.repo, UpstreamName: "codeberg", UpstreamOrg: "acme"}, vmrunner.Requirement{CandidateSHA: original.TestedSHA, BaselineSHA: base, Contract: &contract}, io.Discard); err != nil {
				t.Fatal(err)
			}
			oldEvidence, err := f.store.KVMPreflight(t.Context(), original.ID)
			if err != nil {
				t.Fatal(err)
			}
			next, err := f.store.RecoverKVMPreflight(t.Context(), original.ID)
			if err != nil || next.ID == "" {
				t.Fatalf("promotion not retried: %+v %v", next, err)
			}
			f.jobs.results = []JobResult{{Status: model.RunPassed, Phase: "Succeeded"}}
			head := base
			if moved {
				head = strings.Repeat("e", 40)
			}
			f.git.remoteRefs = map[string]string{"refs/heads/main": head}
			if err = f.scheduler.ProcessNext(t.Context()); err != nil {
				t.Fatal(err)
			}
			after, err := f.store.Run(t.Context(), next.ID)
			if err != nil {
				t.Fatal(err)
			}
			evidence, err := f.store.KVMPreflight(t.Context(), next.ID)
			if err != nil || evidence.Receipt == nil || evidence.Receipt.IntentDigest == oldEvidence.Receipt.IntentDigest {
				t.Fatalf("promotion reused prior evidence: %+v %v", evidence, err)
			}
			old, err := f.store.Promotion(t.Context(), promotion.ID)
			if err != nil || old.Status != model.PromotionInterrupted || old.RunID != original.ID {
				t.Fatalf("original promotion mutated: %+v %v", old, err)
			}
			if moved {
				if len(f.git.promotions) != 0 || after.Status != model.RunFailed || after.Phase != "publishing" {
					t.Fatalf("pushed over moved main: %+v pushes=%v", after, f.git.promotions)
				}
			} else if len(f.git.promotions) != 1 || after.Status != model.RunPassed {
				t.Fatalf("exact authorized retry not delivered once: %+v pushes=%v", after, f.git.promotions)
			}
		})
	}
}
