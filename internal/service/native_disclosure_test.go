package service_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestNativeDiagnosticDisclosureBoundary(t *testing.T) {
	t.Parallel()
	for _, reader := range []actor.Actor{caller(), admin()} {
		for _, typed := range []bool{false, true} {
			t.Run(reader.Identity+fmt.Sprint(typed), func(t *testing.T) { nativeDisclosureCase(t, typed, reader) })
		}
	}
}
func nativeDisclosureCase(t *testing.T, typed bool, reader actor.Actor) {
	t.Helper()
	f := setup(t)
	r := f.seed("failed")
	r.Detail = "other-tenant-secret-SENTINEL node-SENTINEL credential-SENTINEL"
	r.DisplayReason = "image pull failed"
	r.Origin.WorkflowUID = "own-workflow"
	r.Failure = &stepmodel.FailureRecord{Class: stepmodel.FailureInfrastructure, Step: "build", Name: "build", NodeID: "node-SENTINEL", Retry: stepmodel.RetryHint{Action: stepmodel.RetryNone, Detail: r.Detail, ReasonCode: "credential-SENTINEL"}}
	step := stepmodel.RunStep{Path: "build", Name: "build", Status: stepmodel.StepFailed, Detail: r.Detail, DisplayReason: "image pull failed", Logs: []stepmodel.LogSelector{{NodeID: "own-node", PodName: "own-pod", Container: "main"}}}
	if typed {
		diagnostic := &stepmodel.NativeDiagnostic{Cause: "image", Source: "pod", WorkflowUID: "own-workflow", NativeUID: "node-SENTINEL", Constraints: []string{"cpu", "credential-SENTINEL"}}
		r.Diagnostic, step.Diagnostic = diagnostic.Clone(), diagnostic.Clone()
	}
	f.store.native.SeedRun(r)
	f.store.native.SeedSteps(r.ID, []stepmodel.RunStep{step})
	f.store.native.SeedEvent(model.AgentEvent{Repo: repository, RunID: r.ID, Kind: "run.updated", Summary: r.Detail, Data: json.RawMessage(`{"native":"credential-SENTINEL"}`)})
	get, err := f.s.RunGet(t.Context(), reader, viewQuery{ID: r.ID})
	if err != nil {
		t.Fatal(err)
	}
	list, err := f.s.RunList(t.Context(), reader, viewQuery{})
	if err != nil {
		t.Fatal(err)
	}
	wait, err := f.s.RunWait(t.Context(), reader, viewQuery{ID: r.ID}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	events, err := f.s.EventsSince(t.Context(), reader, eventsInput{})
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []any{get, list, wait, events} {
		assertNativeDisclosure(t, value)
	}
	checkProjectedNativeDetail(t, typed, get)

}
func checkProjectedNativeDetail(t *testing.T, typed bool, get apicontract.RunRecord) {
	t.Helper()

	if !typed && get.Detail != "native detail restricted" {
		t.Fatal("opaque detail was not explicitly withheld", get.Detail)
	}
	if typed && (!strings.Contains(get.Detail, "container image") || len(get.Steps[0].Logs) != 1) {
		t.Fatal("safe cause or admitted own reference lost", get)
	}
}

func assertNativeDisclosure(t *testing.T, value any) {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "SENTINEL") {
		t.Fatal("native text or unresolvable reference crossed the public boundary", string(body))
	}
}

func TestDiagnosticProjectionRechecksPreparedRead(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.seed("running")
	call, err := f.s.PrepareRunGet(t.Context(), apicontract.Caller{Actor: caller()}, apicontract.RunGetArgs{ID: runID})
	if err != nil {
		t.Fatal(err)
	}
	f.store.mu.Lock()
	delete(f.store.Actors, caller().Fingerprint)
	f.store.mu.Unlock()
	result, err := call(t.Context())
	if err == nil || result.ID != "" {
		t.Fatal("prepared native view bypassed revoked authority", result, err)
	}
}
