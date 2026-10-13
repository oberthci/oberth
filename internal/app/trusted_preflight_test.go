package app

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/vmrunner"
)

type protectedControllerFixture struct {
	*blockingController
	request   argojob.Request
	createErr error
	failed    bool
	waitRunID string
	cleaned   bool
}

func (c *protectedControllerFixture) Namespace() string { return "test-pipeline" }
func (c *protectedControllerFixture) Create(_ context.Context, r argojob.Request) (string, error) {
	c.request = r
	return r.Name, c.createErr
}
func (c *protectedControllerFixture) Wait(_ context.Context, _ string, runID string, _ io.Writer) (argojob.Completion, error) {
	c.waitRunID = runID
	return argojob.Completion{Succeeded: !c.failed}, nil
}
func (c *protectedControllerFixture) TrustedKVMWorkflowUID(context.Context, argojob.Request) (string, error) {
	if c.createErr != nil {
		return "", errors.New("not observed")
	}
	return "workflow-uid", nil
}
func (c *protectedControllerFixture) CleanupTrustedKVM(context.Context, string, string, string) error {
	c.cleaned = true
	return nil
}
func (c *protectedControllerFixture) TrustedKVMReceipt(_ context.Context, r argojob.Request, uid string) (vmrunner.KVMReceipt, error) {
	intent := *r.TrustedKVM
	d := vmrunner.KVMIntentDigest(intent)
	now := time.Now().UTC()
	return vmrunner.KVMReceipt{IntentDigest: d, WorkflowUID: uid, Binding: vmrunner.KVMTerminationBinding{Namespace: intent.Namespace, WorkflowName: intent.WorkflowName, WorkflowUID: uid, PodName: "pod", PodUID: "pod-uid", ContainerID: "containerd://id", HarnessImageRef: intent.Recipe.HarnessImage, AdmittedAt: intent.AdmittedAt, Deadline: intent.Deadline}, Report: vmrunner.KVMReport{Version: 1, IntentDigest: d, InventoryDigest: intent.InventoryDigest, CaseCount: intent.CaseCount, ArtifactSetDigest: strings.Repeat("f", 64)}, Termination: vmrunner.KVMTerminationRecord{ImageID: "sha256:" + strings.Repeat("c", 64), ContainerID: "containerd://id", PodUID: "pod-uid", WorkflowUID: uid, StartedAt: intent.AdmittedAt, FinishedAt: now}, FinishedAt: now}, nil
}
func TestTrustedPreflightAdapterLifecycle(t *testing.T) {
	for _, scenario := range []string{"pass", "workflow failed", "definite create refusal", "uncertain create", "unapproved"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			fixture := newRaceFixture(t)
			controller := &protectedControllerFixture{blockingController: newBlockingController(), failed: scenario == "workflow failed"}
			if scenario == "definite create refusal" {
				controller.createErr = argojob.ErrTrustedKVMCreateRefused
			}
			if scenario == "uncertain create" {
				controller.createErr = errors.New("transport uncertainty")
			}
			fixture.jobs.controller = controller
			fixture.jobs.config.KVMEnabled = true
			database, err := store.Open(ctx, filepath.Join(t.TempDir(), "db.sqlite"), store.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			upstream, err := database.CreateUpstream(ctx, model.UpstreamSpec{Name: "upstream", Kind: "ssh", BaseURL: "ssh://example.org/org"})
			if err != nil {
				t.Fatal(err)
			}
			repo, err := database.CreateRepository(ctx, model.RepositorySpec{Name: "repo", UpstreamID: upstream.ID, DefaultBranch: "main"})
			if err != nil {
				t.Fatal(err)
			}
			sha := strings.Repeat("a", 40)
			if _, err = database.EnqueueRun(ctx, model.RunSpec{RepoID: repo.ID, RefKind: model.RefBranch, Ref: "feature", SHA: sha, TestedSHA: sha, Actor: "agent", Trigger: "branch"}); err != nil {
				t.Fatal(err)
			}
			run, err := database.ClaimNextRun(ctx)
			if err != nil {
				t.Fatal(err)
			}
			guest := strings.Repeat("b", 64)
			manifest := []byte(fmt.Sprintf("version: 1\nprofile: kvm-suite-amd64-v1\nguest_sha256: %s\nartifacts: [program]\ncases: [one]\n", guest))
			var bundle bytes.Buffer
			tw := tar.NewWriter(&bundle)
			if err = tw.WriteHeader(&tar.Header{Name: "manifest.yaml", Size: int64(len(manifest)), Mode: 0444, Typeflag: tar.TypeReg, Format: tar.FormatUSTAR}); err != nil {
				t.Fatal(err)
			}
			if _, err = tw.Write(manifest); err != nil {
				t.Fatal(err)
			}
			if err = tw.Close(); err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(bundle.Bytes())
			suite := hex.EncodeToString(sum[:])
			recipe := vmrunner.KVMRecipe{SuiteSHA256: suite, GuestSHA256: guest, HarnessImage: "registry.example/harness@sha256:" + strings.Repeat("c", 64), SuiteURL: "https://example.org/suite", GuestURL: "https://example.org/guest"}
			p, err := NewTrustedPreflights(fixture.jobs, database, []vmrunner.KVMRecipe{recipe}, t.TempDir(), func(context.Context) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			fetches := 0
			p.fetch = func(context.Context, string, string, int64) ([]byte, error) { fetches++; return bundle.Bytes(), nil }
			contract := vmrunner.Contract{Version: 2, Profile: vmrunner.KVMSuiteAMD64Profile, SuiteSHA256: suite, GuestSHA256: guest, Artifacts: []string{"program"}}
			if scenario == "unapproved" {
				contract.SuiteSHA256 = strings.Repeat("e", 64)
			}
			err = p.Execute(ctx, service.JobRequest{JobName: "run-test", Run: run, Repository: repo, UpstreamName: "upstream", UpstreamOrg: "org", SourceDir: fixture.sourceDir}, vmrunner.Requirement{CandidateSHA: sha, BaselineSHA: strings.Repeat("d", 40), Contract: &contract}, io.Discard)
			if scenario == "unapproved" {
				if err == nil || fetches != 0 {
					t.Fatal("unapproved content fetched")
				}
				return
			}
			state, getErr := database.KVMPreflight(ctx, run.ID)
			if getErr != nil {
				t.Fatalf("preflight did not journal: execute=%v journal=%v", err, getErr)
			}
			if scenario == "pass" {
				if err != nil || state.Receipt == nil || state.Refusal != nil || !state.Cleaned || controller.waitRunID != run.ID {
					t.Fatalf("success binding: %v %#v waitID=%q", err, state, controller.waitRunID)
				}
			} else {
				if err == nil || state.Refusal == nil || state.Receipt != nil {
					t.Fatalf("failure lost negative receipt: %v %#v", err, state)
				}
				if scenario == "uncertain create" {
					if state.Cleaned {
						t.Fatal("uncertain create released capacity")
					}
				} else if !state.Cleaned {
					t.Fatal("definite failure not cleaned")
				}
			}
		})
	}
}
