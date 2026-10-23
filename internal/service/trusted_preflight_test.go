package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/vmrunner"
)

type protectedFixture struct {
	store                                  *store.Store
	fail, omitReceipt, omitCleanup, revoke bool
	intent                                 vmrunner.KVMIntent
	recoverIDs                             []string
	recoverErr                             error
	artifactDigest                         string
	failure                                *TrustedPreflightFailure
}

func (p *protectedFixture) Supports(vmrunner.Contract) error { return nil }
func (p *protectedFixture) RecipeAllowed(vmrunner.KVMRecipe) error {
	if p.revoke {
		return errors.New("recipe revoked")
	}
	return nil
}
func (p *protectedFixture) Recover(context.Context) ([]string, error) {
	return p.recoverIDs, p.recoverErr
}
func (p *protectedFixture) Execute(ctx context.Context, job JobRequest, requirement vmrunner.Requirement, output io.Writer) error {
	run, err := p.store.Run(ctx, job.Run.ID)
	if err != nil {
		return err
	}
	if run.JobName != "" {
		return errors.New("main delivery occurred before suite")
	}
	if p.fail {
		return errors.New("host driver failed")
	}
	now := time.Now().UTC()
	contract := *requirement.Contract
	p.intent = vmrunner.KVMIntent{RunID: run.ID, RepoID: run.RepoID, Repository: job.UpstreamName + "/" + job.UpstreamOrg + "/" + job.Repository.Name, CandidateSHA: requirement.CandidateSHA, BaselineSHA: requirement.BaselineSHA, Contract: contract, Recipe: vmrunner.KVMRecipe{SuiteSHA256: contract.SuiteSHA256, GuestSHA256: contract.GuestSHA256, HarnessImage: "registry.example/harness@sha256:" + strings.Repeat("e", 64), SuiteURL: "https://example.org/suite", GuestURL: "https://example.org/guest"}, Namespace: "pipelines", WorkflowName: "trusted-" + run.ID, WorkflowSpecDigest: strings.Repeat("f", 64), InventoryDigest: strings.Repeat("a", 64), CaseCount: 1, AdmittedAt: now, Deadline: now.Add(time.Minute)}
	if _, err = p.store.ReserveKVMPreflight(ctx, p.intent); err != nil {
		return err
	}
	if err = p.store.SubmitKVMPreflight(ctx, run.ID); err != nil {
		return err
	}
	if err = p.store.BindKVMPreflight(ctx, run.ID, "workflow"); err != nil {
		return err
	}
	if p.failure != nil {
		if _, err := fmt.Fprintln(output, "[test/unit] trusted step diagnostic"); err != nil {
			return err
		}
		if err := p.store.RecordKVMRefusal(ctx, run.ID, vmrunner.KVMRefusal{Code: "preflight_failed", ObservedAt: now}); err != nil {
			return err
		}
		if err := p.store.CompleteKVMPreflightCleanup(ctx, run.ID, "workflow"); err != nil {
			return err
		}
		return p.failure
	}
	artifactDigest := p.artifactDigest
	if artifactDigest == "" {
		artifactDigest = strings.Repeat("b", 64)
	}
	digest := vmrunner.KVMIntentDigest(p.intent)
	receipt := vmrunner.KVMReceipt{IntentDigest: digest, WorkflowUID: "workflow", Binding: vmrunner.KVMTerminationBinding{Namespace: "pipelines", WorkflowName: p.intent.WorkflowName, WorkflowUID: "workflow", PodName: "pod", PodUID: "poduid", ContainerID: "containerd://id", HarnessImageRef: p.intent.Recipe.HarnessImage, AdmittedAt: now, Deadline: p.intent.Deadline}, Report: vmrunner.KVMReport{Version: 1, IntentDigest: digest, InventoryDigest: p.intent.InventoryDigest, ArtifactSetDigest: artifactDigest, CaseCount: 1}, Termination: vmrunner.KVMTerminationRecord{ImageID: "sha256:" + strings.Repeat("e", 64), ContainerID: "containerd://id", PodUID: "poduid", WorkflowUID: "workflow", StartedAt: now, FinishedAt: now}, FinishedAt: now}
	if !p.omitReceipt {
		if err = p.store.RecordKVMReceipt(ctx, run.ID, receipt); err != nil {
			return err
		}
	}
	if !p.omitCleanup {
		return p.store.CompleteKVMPreflightCleanup(ctx, run.ID, "workflow")
	}
	return errors.New("cleanup uncertain")
}
func TestTrustedPreflightGatesReleaseBeforeCredentials(t *testing.T) {
	for _, mode := range []string{"pass", "driver failure", "missing receipt", "uncertain cleanup", "revoked recipe"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			fixture := newControlFixture(t, JobResult{Status: model.RunPassed, Phase: "Succeeded"})
			contract := []byte("version: 2\nprofile: kvm-suite-amd64-v1\nsuite_sha256: " + strings.Repeat("a", 64) + "\nguest_sha256: " + strings.Repeat("b", 64) + "\nartifacts: [build/program]\n")
			fixture.scheduler.git = &policyTestGit{controlGit: fixture.git, candidate: contract, baseline: contract}
			executor := &protectedFixture{store: fixture.store, fail: mode == "driver failure", omitReceipt: mode == "missing receipt", omitCleanup: mode == "uncertain cleanup", revoke: mode == "revoked recipe"}
			fixture.scheduler.trusted = executor
			queued, err := fixture.scheduler.AdmitRelease(ctx, ReleaseRequest{EventID: "trusted-release", Repository: fixture.repo, Tag: "v1.0.0", ObjectSHA: strings.Repeat("d", 40), CommitSHA: strings.Repeat("c", 40), Actor: "agent@host"})
			if err != nil {
				t.Fatal(err)
			}
			err = fixture.scheduler.ProcessNext(ctx)
			run, getErr := fixture.store.Run(ctx, queued.ID)
			if getErr != nil {
				t.Fatal(getErr)
			}
			if mode == "pass" {
				if err != nil || len(fixture.jobs.createdReleases) != 1 || run.Status != model.RunPassed {
					t.Fatalf("valid preflight did not release: %v %#v", err, run)
				}
				return
			}
			if len(fixture.jobs.createdReleases) != 0 || len(fixture.jobs.createdCI) != 0 || run.JobName != "" {
				t.Fatalf("suite failure delivered credentials/workflow: %#v", run)
			}
			if mode == "uncertain cleanup" {
				if err == nil || run.Status != model.RunRunning {
					t.Fatalf("uncertain cleanup lost active recovery: %v %#v", err, run)
				}
			} else if err != nil || run.Status != model.RunFailed {
				t.Fatalf("definitive refusal: %v %#v", err, run)
			}
		})
	}
}
