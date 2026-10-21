package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/vmrunner"
)

type alteredReceiptStore struct {
	*store.Store
	alter func(*vmrunner.KVMExecution)
}

func (s *alteredReceiptStore) KVMPreflight(ctx context.Context, id string) (vmrunner.KVMExecution, error) {
	execution, err := s.Store.KVMPreflight(ctx, id)
	if err == nil {
		s.alter(&execution)
	}
	return execution, err
}

func trustedPublicationFixture(t *testing.T, release, cleaned bool) (*controlFixture, *protectedFixture, model.Run) {
	t.Helper()
	ctx := context.Background()
	f := newControlFixture(t)
	raw := []byte("version: 2\nprofile: kvm-suite-amd64-v1\nsuite_sha256: " + strings.Repeat("a", 64) + "\nguest_sha256: " + strings.Repeat("b", 64) + "\nartifacts: [build/program]\n")
	contract, err := vmrunner.DecodeContract(raw)
	if err != nil {
		t.Fatal(err)
	}
	f.scheduler.git = &policyTestGit{controlGit: f.git, candidate: raw, baseline: raw}
	executor := &protectedFixture{store: f.store, omitCleanup: !cleaned}
	f.scheduler.trusted = executor
	spec := model.RunSpec{RepoID: f.repo.ID, RefKind: model.RefBranch, Ref: "feature/trusted", SHA: strings.Repeat("c", 40), TestedSHA: strings.Repeat("c", 40), Actor: "agent", Trigger: "branch"}
	if release {
		spec.RefKind = model.RefTag
		spec.Ref = "v1.0.0"
		spec.SHA = strings.Repeat("d", 40)
		spec.Release = true
		spec.Credentialed = true
		spec.Trigger = "tag"
	}
	if _, err = f.store.EnqueueRun(ctx, spec); err != nil {
		t.Fatal(err)
	}
	run, err := f.store.ClaimNextRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = executor.Execute(ctx, JobRequest{Run: run, Repository: f.repo, UpstreamName: "codeberg", UpstreamOrg: "acme"}, vmrunner.Requirement{CandidateSHA: run.TestedSHA, BaselineSHA: strings.Repeat("b", 40), Contract: &contract}, io.Discard)
	if cleaned && err != nil {
		t.Fatal(err)
	}
	return f, executor, run
}

func TestTrustedPublicationGateRejectsMismatchedEvidence(t *testing.T) {
	for _, mode := range []string{"valid branch", "valid release", "missing receipt", "candidate mismatch", "baseline mismatch", "revoked recipe", "release object mismatch", "fast forward newer failed run"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			release := strings.Contains(mode, "release")
			f, executor, run := trustedPublicationFixture(t, release, true)
			var err error
			run, err = f.store.FinishRun(ctx, run.ID, model.RunResult{Status: model.RunPassed, Phase: "passed"})
			if err != nil {
				t.Fatal(err)
			}
			publication := model.Publication{RunID: run.ID, ResultSHA: run.SHA}
			switch mode {
			case "missing receipt", "candidate mismatch", "baseline mismatch":
				f.scheduler.store = &alteredReceiptStore{Store: f.store, alter: func(e *vmrunner.KVMExecution) {
					switch mode {
					case "missing receipt":
						e.Receipt = nil
					case "candidate mismatch":
						e.Intent.CandidateSHA = strings.Repeat("f", 40)
					case "baseline mismatch":
						e.Intent.BaselineSHA = strings.Repeat("f", 40)
					}
				}}
			case "revoked recipe":
				executor.revoke = true
			case "release object mismatch":
				publication.ResultSHA = run.TestedSHA
			case "fast forward newer failed run":
				if _, err = f.store.EnqueueRun(ctx, model.RunSpec{RepoID: f.repo.ID, RefKind: model.RefBranch, Ref: "feature/newer", SHA: run.TestedSHA, TestedSHA: run.TestedSHA, Actor: "agent", Trigger: "branch"}); err != nil {
					t.Fatal(err)
				}
				newer, err := f.store.ClaimNextRun(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = f.store.FinishRun(ctx, newer.ID, model.RunResult{Status: model.RunFailed, Phase: "failed"}); err != nil {
					t.Fatal(err)
				}
				latest, err := f.store.LatestRunForSHA(ctx, f.repo.ID, run.TestedSHA)
				if err != nil || latest.ID != newer.ID {
					t.Fatalf("newer failed fixture: %#v %v", latest, err)
				}
				publication.RunID = ""
			}
			err = f.scheduler.requirePublicationTestPolicy(ctx, f.repo, publication)
			if strings.HasPrefix(mode, "valid ") {
				if err != nil {
					t.Fatalf("valid evidence refused: %v", err)
				}
			} else if err == nil {
				t.Fatal("publication admitted mismatched/missing proof")
			}
			if len(f.jobs.createdCI) != 0 || len(f.jobs.createdReleases) != 0 || len(f.git.syncedBranches) != 0 || len(f.git.syncedTags) != 0 {
				t.Fatal("gate delivered external side effect")
			}
		})
	}
}

func TestTrustedPreflightSchedulerStartupRecovery(t *testing.T) {
	for _, mode := range []string{"one recovery error", "cleaned undelivered", "cleaned despite another error", "missing executor"} {
		t.Run(mode, func(t *testing.T) {
			cleaned := mode != "one recovery error"
			f, executor, run := trustedPublicationFixture(t, false, cleaned)
			if cleaned {
				executor.recoverIDs = []string{run.ID}
			}
			if mode == "one recovery error" || mode == "cleaned despite another error" {
				executor.recoverErr = errors.New("one preflight API observation unavailable")
			}
			if mode == "missing executor" {
				f.scheduler.trusted = nil
			}
			// Startup recovery runs to completion on its own: the run loop is
			// stopped only after the scheduler reports recovery finished, so
			// no wall-clock deadline can cut recovery short on a loaded node.
			// The outer bound only keeps a hung scheduler from hanging the
			// test.
			recovered := make(chan struct{})
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			f.scheduler.onStartupRecovered = func() { close(recovered); <-ctx.Done() }
			defer cancel()
			result := make(chan error, 1)
			go func() { result <- f.scheduler.Run(ctx) }()
			var err error
			select {
			case <-recovered:
				cancel()
				err = <-result
				if mode == "missing executor" {
					t.Fatal("pending row without executor started")
				}
				if err != nil && !errors.Is(err, context.Canceled) {
					t.Fatalf("scheduler stopped with %v after startup recovery", err)
				}
			case err = <-result:
				if mode == "missing executor" {
					if err == nil {
						t.Fatal("pending row without executor started")
					}
					return
				}
				t.Fatalf("individual recovery error stopped scheduler: %v", err)
			}
			got, err := f.store.Run(context.Background(), run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if cleaned {
				if got.Status != model.RunInterrupted || !strings.Contains(got.Error, "preflight interrupted") || got.SupersededBy == "" {
					t.Fatalf("undelivered parent not interrupted: %#v", got)
				}
				retry, err := f.store.Run(context.Background(), got.SupersededBy)
				if err != nil || retry.Status != model.RunQueued || retry.JobName != "" || retry.AdmissionSequence != got.AdmissionSequence || retry.SHA != got.SHA {
					t.Fatalf("fresh retry not durably gated: %+v %v", retry, err)
				}
				if _, err := f.store.KVMPreflight(context.Background(), retry.ID); !errors.Is(err, store.ErrNotFound) {
					t.Fatal("old evidence reused for fresh retry")
				}

			} else if got.Status != model.RunRunning {
				t.Fatalf("uncertain obligation lost: %#v", got)
			}
			if len(f.jobs.createdCI) != 0 || len(f.jobs.createdReleases) != 0 || len(f.git.syncedBranches) != 0 || len(f.git.syncedTags) != 0 {
				t.Fatal("restart delivered parent without rerun")
			}
		})
	}
}

// The release rebuild is a separate producer. Its bytes cannot rewrite the
// receipt of the immutable credential-free preflight artifact snapshot.
func TestTrustedReceiptCertifiesPreflightNotRebuiltReleaseBytes(t *testing.T) {
	ctx := context.Background()
	f := newControlFixture(t, JobResult{Status: model.RunPassed, Phase: "Succeeded"})
	raw := []byte("version: 2\nprofile: kvm-suite-amd64-v1\nsuite_sha256: " + strings.Repeat("a", 64) + "\nguest_sha256: " + strings.Repeat("b", 64) + "\nartifacts: [build/program]\n")
	f.scheduler.git = &policyTestGit{controlGit: f.git, candidate: raw, baseline: raw}
	preflight := sha256.Sum256([]byte("preflight artifact snapshot"))
	executor := &protectedFixture{store: f.store, artifactDigest: hex.EncodeToString(preflight[:])}
	f.scheduler.trusted = executor
	rebuiltPath := filepath.Join(t.TempDir(), "release-binary")
	rebuilt := []byte("independently rebuilt release bytes")
	f.scheduler.jobs = &policyChangingJobs{fakeJobs: f.jobs, afterBuild: func() {
		if err := os.WriteFile(rebuiltPath, rebuilt, 0600); err != nil {
			t.Fatal(err)
		}
	}}
	queued, err := f.scheduler.AdmitRelease(ctx, ReleaseRequest{EventID: "byte-scope", Repository: f.repo, Tag: "v1.0.0", ObjectSHA: strings.Repeat("d", 40), CommitSHA: strings.Repeat("c", 40), Actor: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	if err = f.scheduler.ProcessNext(ctx); err != nil {
		t.Fatal(err)
	}
	run, err := f.store.Run(ctx, queued.ID)
	if err != nil || run.Status != model.RunPassed || len(f.jobs.createdReleases) != 1 {
		t.Fatalf("preflight failed to gate release: %#v %v", run, err)
	}
	bytes, err := os.ReadFile(rebuiltPath)
	if err != nil {
		t.Fatal(err)
	}
	rebuiltDigest := sha256.Sum256(bytes)
	evidence, err := f.store.KVMPreflight(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Receipt.Report.ArtifactSetDigest != executor.artifactDigest || evidence.Receipt.Report.ArtifactSetDigest == hex.EncodeToString(rebuiltDigest[:]) {
		t.Fatal("receipt must retain preflight bytes, never certify rebuilt bytes")
	}
	if err = f.scheduler.requirePublicationTestPolicy(ctx, f.repo, model.Publication{RunID: run.ID, ResultSHA: run.SHA}); err != nil {
		t.Fatalf("source/test gate unexpectedly claims rebuilt-byte equality: %v", err)
	}
}
