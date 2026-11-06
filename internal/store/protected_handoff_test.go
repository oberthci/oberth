package store

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/protectedtests"
)

func TestProtectedHandoffRequiresCleanupAndSurvivesJournalReopen(t *testing.T) {
	ctx := t.Context()
	now := time.Now().UTC()
	path := filepath.Join(t.TempDir(), "handoff.db")
	options := Options{Now: func() time.Time { return now }}
	s, err := Open(ctx, path, options)
	requireStoreNoError(t, err)
	// Close the currently open handle even if an assertion interrupts a phase.
	defer func() {
		if s != nil {
			_ = s.Close()
		}
	}()
	intent := kvmIntent(t, s)
	want := protectedtests.HandoffExpectation{RunID: intent.RunID, RepoID: intent.RepoID, Repository: intent.Repository, CandidateSHA: intent.CandidateSHA, BaselineSHA: intent.BaselineSHA, Contract: &intent.Contract}
	recipes := []protectedtests.Recipe{intent.Recipe}
	approve := func(r protectedtests.Recipe) error { return protectedtests.RecipeAllowed(r, recipes) }
	_, err = s.ReserveKVMPreflight(ctx, intent)
	requireStoreNoError(t, err)
	requireStoreNoError(t, s.SubmitKVMPreflight(ctx, intent.RunID))
	requireStoreNoError(t, s.BindKVMPreflight(ctx, intent.RunID, "workflow-uid"))
	requireStoreNoError(t, s.RecordKVMReceipt(ctx, intent.RunID, kvmReceipt(intent)))
	state, err := s.KVMPreflight(ctx, intent.RunID)
	requireStoreNoError(t, err)
	if err = state.ValidateHandoff(want, approve); err == nil {
		t.Fatal("receipt alone authorized handoff before cleanup")
	}
	_, err = s.SetRunWorkflowName(ctx, intent.RunID, "consumer-workflow")
	requireKVMStateError(t, err)
	requireStoreNoError(t, s.CompleteKVMPreflightCleanup(ctx, intent.RunID, "workflow-uid"))
	state, err = s.KVMPreflight(ctx, intent.RunID)
	requireStoreNoError(t, err)
	requireStoreNoError(t, state.ValidateHandoff(want, approve))
	before, err := json.Marshal(state)
	requireStoreNoError(t, err)
	requireStoreNoError(t, s.Close())
	s, err = Open(ctx, path, options)
	requireStoreNoError(t, err)
	state, err = s.KVMPreflight(ctx, intent.RunID)
	requireStoreNoError(t, err)
	after, err := json.Marshal(state)
	requireStoreNoError(t, err)
	if string(before) != string(after) {
		t.Fatal("restart changed durable handoff evidence")
	}
	recipes = nil
	if err = state.ValidateHandoff(want, approve); err == nil {
		t.Fatal("restart reused revoked recipe authority")
	}
	recipes = []protectedtests.Recipe{intent.Recipe}
	requireStoreNoError(t, state.ValidateHandoff(want, approve))
	_, err = s.SetRunWorkflowName(ctx, intent.RunID, "consumer-workflow")
	requireStoreNoError(t, err)
	_, err = s.VerifyAuditState(ctx)
	requireStoreNoError(t, err)
}
