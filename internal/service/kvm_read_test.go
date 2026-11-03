package service

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/vmrunner"
)

func kvmReadFixture(t *testing.T) (*controlFixture, *API, vmrunner.KVMIntent) {
	t.Helper()
	f := newControlFixture(t)
	ctx := context.Background()
	queued, err := f.scheduler.EnqueueCI(ctx, CIRequest{EventID: "kvm-read", Repository: f.repo, Branch: "feature/kvm-read", SHA: strings.Repeat("a", 40), Actor: "agent@host"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := f.store.ClaimNextRun(ctx)
	if err != nil || run.ID != queued.ID {
		t.Fatalf("claim: %v", err)
	}
	now := time.Now().UTC()
	intent := vmrunner.KVMIntent{RunID: run.ID, RepoID: run.RepoID, Repository: "codeberg/acme/oberth", CandidateSHA: run.TestedSHA, BaselineSHA: strings.Repeat("b", 40), Contract: vmrunner.Contract{Version: 2, Profile: vmrunner.KVMSuiteAMD64Profile, SuiteSHA256: strings.Repeat("c", 64), GuestSHA256: strings.Repeat("d", 64), Artifacts: []string{"build/program"}}, Recipe: vmrunner.KVMRecipe{SuiteSHA256: strings.Repeat("c", 64), GuestSHA256: strings.Repeat("d", 64), HarnessImage: "registry.example/harness@sha256:" + strings.Repeat("e", 64), SuiteURL: "https://example.org/suite", GuestURL: "https://example.org/guest"}, Namespace: "pipelines", WorkflowName: "trusted-" + run.ID, WorkflowSpecDigest: strings.Repeat("f", 64), InventoryDigest: strings.Repeat("a", 64), CaseCount: 2, AdmittedAt: now, Deadline: now.Add(time.Minute)}
	control, err := NewAPI(APIConfig{Runs: f.store, History: f.store, Repositories: f.store, KVMPreflights: f.store, MutationGate: func(context.Context) error { return errors.New("mutation forbidden") }})
	if err != nil {
		t.Fatal(err)
	}
	return f, control, intent
}

func readKVM(t *testing.T, f *controlFixture, control *API, id string) KVMPreflightResponse {
	t.Helper()
	ctx := context.Background()
	before, err := f.store.VerifyAuditChain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]string{"id": id})
	value, err := control.CallTool(ctx, api.Actor{Identity: "reader@host"}, "run_kvm_preflight", raw)
	if err != nil {
		t.Fatal(err)
	}
	after, err := f.store.VerifyAuditChain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("historical read mutated audit state")
	}
	response, ok := value.(KVMPreflightResponse)
	if !ok {
		t.Fatalf("unexpected DTO %T", value)
	}
	body, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "may_publish") || strings.Contains(string(body), `"passed":`) {
		t.Fatal("read invented publication authority")
	}
	return response
}

func readReceipt(intent vmrunner.KVMIntent) vmrunner.KVMReceipt {
	digest := vmrunner.KVMIntentDigest(intent)
	return vmrunner.KVMReceipt{IntentDigest: digest, WorkflowUID: "workflow-uid", Binding: vmrunner.KVMTerminationBinding{Namespace: intent.Namespace, WorkflowName: intent.WorkflowName, WorkflowUID: "workflow-uid", PodName: "pod", PodUID: "pod-uid", ContainerID: "containerd://id", HarnessImageRef: intent.Recipe.HarnessImage, AdmittedAt: intent.AdmittedAt, Deadline: intent.Deadline}, Report: vmrunner.KVMReport{Version: 1, IntentDigest: digest, InventoryDigest: intent.InventoryDigest, ArtifactSetDigest: strings.Repeat("b", 64), CaseCount: intent.CaseCount}, Termination: vmrunner.KVMTerminationRecord{ImageID: "sha256:" + strings.Repeat("e", 64), ContainerID: "containerd://id", PodUID: "pod-uid", WorkflowUID: "workflow-uid", StartedAt: intent.AdmittedAt, FinishedAt: intent.AdmittedAt}, FinishedAt: intent.AdmittedAt}
}

func TestKVMReadHistoricalReceiptAndParentAreSeparate(t *testing.T) {
	f, control, intent := kvmReadFixture(t)
	ctx := context.Background()
	absent := readKVM(t, f, control, intent.RunID[:12])
	if absent.Present || absent.Evidence != nil || absent.RunID != intent.RunID || absent.Repository != intent.Repository || absent.CandidateSHA != intent.CandidateSHA {
		t.Fatalf("absence or canonical binding: %#v", absent)
	}
	if _, err := f.store.ReserveKVMPreflight(ctx, intent); err != nil {
		t.Fatal(err)
	}
	reserved := readKVM(t, f, control, intent.RunID)
	if !reserved.Present || reserved.Evidence.ReceiptValid || reserved.Evidence.Submitted {
		t.Fatalf("reserved: %#v", reserved)
	}
	if err := f.store.SubmitKVMPreflight(ctx, intent.RunID); err != nil {
		t.Fatal(err)
	}
	if err := f.store.BindKVMPreflight(ctx, intent.RunID, "workflow-uid"); err != nil {
		t.Fatal(err)
	}
	receipt := readReceipt(intent)
	if err := f.store.RecordKVMReceipt(ctx, intent.RunID, receipt); err != nil {
		t.Fatal(err)
	}
	got := readKVM(t, f, control, intent.RunID)
	if !got.Evidence.ReceiptValid || got.Evidence.Cleaned || !reflect.DeepEqual(*got.Evidence.Receipt, receipt) {
		t.Fatalf("receipt before cleanup: %#v", got)
	}
	if err := f.store.CompleteKVMPreflightCleanup(ctx, intent.RunID, "workflow-uid"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.FinishRun(ctx, intent.RunID, model.RunResult{Status: model.RunInterrupted, Error: "restart interrupted parent"}); err != nil {
		t.Fatal(err)
	}
	// No current recipe is wired into this read service. Historical validity
	// survives policy changes and never overrides the interrupted parent.
	got = readKVM(t, f, control, intent.RunID[:12])
	if !got.Evidence.ReceiptValid || !got.Evidence.Cleaned || got.ParentStatus != model.RunInterrupted {
		t.Fatalf("historical receipt became parent verdict: %#v", got)
	}
}

func TestKVMReadCleanupWithoutReceiptAndRefusal(t *testing.T) {
	for _, scenario := range []string{"never submitted", "create refused", "failed termination"} {
		t.Run(scenario, func(t *testing.T) {
			f, control, intent := kvmReadFixture(t)
			ctx := context.Background()
			if _, err := f.store.ReserveKVMPreflight(ctx, intent); err != nil {
				t.Fatal(err)
			}
			uid := ""
			if scenario != "never submitted" {
				if err := f.store.SubmitKVMPreflight(ctx, intent.RunID); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "create refused" {
				if err := f.store.RejectKVMPreflight(ctx, intent.RunID); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "failed termination" {
				uid = "workflow-uid"
				if err := f.store.BindKVMPreflight(ctx, intent.RunID, uid); err != nil {
					t.Fatal(err)
				}
				refusal := vmrunner.KVMRefusal{Code: "preflight_failed", ObservedAt: time.Now().UTC(), Termination: &vmrunner.KVMTerminationRecord{WorkflowUID: uid, ExitCode: 23, Signal: 9, RestartCount: 1}}
				if err := f.store.RecordKVMRefusal(ctx, intent.RunID, refusal); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.store.CompleteKVMPreflightCleanup(ctx, intent.RunID, uid); err != nil {
				t.Fatal(err)
			}
			got := readKVM(t, f, control, intent.RunID)
			if !got.Evidence.Cleaned || got.Evidence.ReceiptValid || got.Evidence.Receipt != nil {
				t.Fatalf("cleanup asserted success: %#v", got)
			}
			if scenario == "failed termination" {
				term := got.Evidence.Refusal.Termination
				if term.ExitCode != 23 || term.Signal != 9 || term.RestartCount != 1 {
					t.Fatal("negative termination evidence lost")
				}
			}
		})
	}
}

type kvmReadFake struct {
	value vmrunner.KVMExecution
	err   error
}

func (f kvmReadFake) KVMPreflight(context.Context, string) (vmrunner.KVMExecution, error) {
	return f.value, f.err
}

func TestKVMReadRejectsCorruptBindingAndLifecycle(t *testing.T) {
	f, control, intent := kvmReadFixture(t)
	original := vmrunner.KVMExecution{Intent: intent, WorkflowUID: "workflow-uid", Submitted: true}
	receipt := readReceipt(intent)
	original.Receipt = &receipt
	attacks := map[string]func(*vmrunner.KVMExecution){
		"run":               func(v *vmrunner.KVMExecution) { v.Intent.RunID = strings.Repeat("f", 32) },
		"repo-id":           func(v *vmrunner.KVMExecution) { v.Intent.RepoID++ },
		"qualified-repo":    func(v *vmrunner.KVMExecution) { v.Intent.Repository = "other/acme/oberth" },
		"candidate":         func(v *vmrunner.KVMExecution) { v.Intent.CandidateSHA = strings.Repeat("f", 40) },
		"invalid-intent":    func(v *vmrunner.KVMExecution) { v.Intent.CaseCount = 0 },
		"oversized-intent":  func(v *vmrunner.KVMExecution) { v.Intent.Namespace = strings.Repeat("x", 16385) },
		"oversized-receipt": func(v *vmrunner.KVMExecution) { v.Receipt.Binding.PodName = strings.Repeat("x", 16385) },
		"unsubmitted-uid":   func(v *vmrunner.KVMExecution) { v.Submitted = false },
		"rejected-uid":      func(v *vmrunner.KVMExecution) { v.Rejected = true },
		"receipt-no-uid":    func(v *vmrunner.KVMExecution) { v.WorkflowUID = "" },
		"oversized-uid":     func(v *vmrunner.KVMExecution) { v.WorkflowUID = strings.Repeat("x", 129) },
		"wrong-receipt-uid": func(v *vmrunner.KVMExecution) { v.Receipt.WorkflowUID = "other" },
		"wrong-artifact":    func(v *vmrunner.KVMExecution) { v.Receipt.Report.ArtifactSetDigest = "invalid" },
		"receipt-nonzero":   func(v *vmrunner.KVMExecution) { v.Receipt.Termination.ExitCode = 1 },
		"receipt-restart":   func(v *vmrunner.KVMExecution) { v.Receipt.Termination.RestartCount = 1 },
		"receipt-and-refusal": func(v *vmrunner.KVMExecution) {
			v.Refusal = &vmrunner.KVMRefusal{Code: "canceled", ObservedAt: intent.AdmittedAt}
		},
		"invalid-refusal": func(v *vmrunner.KVMExecution) {
			v.Receipt = nil
			v.Refusal = &vmrunner.KVMRefusal{Code: "arbitrary-payload", ObservedAt: intent.AdmittedAt}
		},
		"oversized-refusal": func(v *vmrunner.KVMExecution) {
			v.Receipt = nil
			v.Refusal = &vmrunner.KVMRefusal{Code: "preflight_failed", ObservedAt: intent.AdmittedAt, Termination: &vmrunner.KVMTerminationRecord{WorkflowUID: v.WorkflowUID, ImageID: strings.Repeat("<", 1024)}}
		},
		"refusal-before-admission": func(v *vmrunner.KVMExecution) {
			v.Receipt = nil
			v.Refusal = &vmrunner.KVMRefusal{Code: "canceled", ObservedAt: intent.AdmittedAt.Add(-time.Second)}
		},
		"refusal-wrong-uid": func(v *vmrunner.KVMExecution) {
			v.Receipt = nil
			v.Refusal = &vmrunner.KVMRefusal{Code: "preflight_failed", ObservedAt: intent.AdmittedAt, Termination: &vmrunner.KVMTerminationRecord{WorkflowUID: "other"}}
		},
		"cleaned-uncertain-create": func(v *vmrunner.KVMExecution) { v.Receipt = nil; v.WorkflowUID = ""; v.Cleaned = true },
		"rejected-unsubmitted": func(v *vmrunner.KVMExecution) {
			v.Receipt = nil
			v.WorkflowUID = ""
			v.Submitted = false
			v.Rejected = true
		},
	}
	for name, attack := range attacks {
		t.Run(name, func(t *testing.T) {
			body, _ := json.Marshal(original)
			var changed vmrunner.KVMExecution
			if err := json.Unmarshal(body, &changed); err != nil {
				t.Fatal(err)
			}
			attack(&changed)
			control.kvmPreflights = kvmReadFake{value: changed}
			_, err := control.readKVMPreflight(context.Background(), intent.RunID)
			if !errors.Is(err, store.ErrInvalidState) {
				t.Fatalf("corrupt evidence accepted: %v", err)
			}
		})
	}
	control.kvmPreflights = f.store
	for _, raw := range []string{`{}`, `{"id":""}`, `{"id":"short"}`, `{"id":"zzzzzzzzzzzz"}`, `{"id":"` + intent.RunID + `","extra":true}`} {
		if _, err := control.CallTool(context.Background(), api.Actor{Identity: "reader@host"}, "run_kvm_preflight", json.RawMessage(raw)); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("arguments %s: %v", raw, err)
		}
	}
	if _, err := control.CallTool(context.Background(), api.Actor{}, "run_kvm_preflight", json.RawMessage(`{"id":"`+intent.RunID+`"}`)); err == nil {
		t.Fatal("unauthenticated read accepted")
	}
	if _, err := control.readKVMPreflight(context.Background(), strings.Repeat("f", 32)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown run: %v", err)
	}
	control.kvmPreflights = nil
	if _, err := control.readKVMPreflight(context.Background(), intent.RunID); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("missing reader: %v", err)
	}
}

// Prefix ambiguity must remain an error, not select a different receipt.
type ambiguousKVMHistory struct{ RunHistory }

func (h ambiguousKVMHistory) ResolveRunID(context.Context, string) (model.Run, error) {
	return model.Run{}, store.ErrAmbiguous
}
func TestKVMReadPreservesAmbiguousPrefix(t *testing.T) {
	_, control, intent := kvmReadFixture(t)
	control.history = ambiguousKVMHistory{RunHistory: control.history}
	if _, err := control.readKVMPreflight(context.Background(), intent.RunID[:12]); !errors.Is(err, store.ErrAmbiguous) {
		t.Fatalf("ambiguous prefix: %v", err)
	}
}
