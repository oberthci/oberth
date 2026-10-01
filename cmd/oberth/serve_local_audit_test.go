package main

import (
	"context"
	"io"
	"path/filepath"
	"testing"

	"github.com/oberthci/oberth/internal/auditanchor"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

// localAuditContinuity is a self-contained continuity fake that records every
// call, proving the default (local-only) startup never touches, prepares, or
// reconciles rollback-external witness state beyond the mandatory empty-genesis
// emptiness proof.
type localAuditContinuity struct {
	intentsCalls   int
	pinnedCalls    int
	prepareCalls   int
	reconcileCalls int
}

func (continuity *localAuditContinuity) Intents(context.Context) ([]model.AuditWitnessIntent, error) {
	continuity.intentsCalls++
	return nil, nil
}

func (continuity *localAuditContinuity) Pinned(context.Context) ([]model.AuditWitness, error) {
	continuity.pinnedCalls++
	return nil, nil
}

func (continuity *localAuditContinuity) Prepare(context.Context, model.AuditWitnessIntent) error {
	continuity.prepareCalls++
	return nil
}

func (continuity *localAuditContinuity) Reconcile(context.Context, []model.AuditWitness) error {
	continuity.reconcileCalls++
	return nil
}

// TestFreshInstallWithoutExternalAnchoringStartsClean walks the exact audit
// startup sequence serve uses — default flags, genesis database creation, the
// startup verifier, the runtime manager, Initialize, Ready, the mutation gate
// — with the default empty Rekor and TSA URLs. No external witness or
// timestamp authority object exists at all, so no external call can happen,
// and the local hash chain still initializes, gates, and appends.
func TestFreshInstallWithoutExternalAnchoringStartsClean(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	options, err := parseServeOptions([]string{"--argo-namespace", "argo-pipelines"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if options.auditTSAURL != "" || options.auditRekorURL != "" {
		t.Fatalf("default audit URLs must be empty, got %q / %q", options.auditTSAURL, options.auditRekorURL)
	}

	// Mirror serve's manager assembly for the disabled defaults: neither
	// Authority nor Witness is ever constructed, so the config carries only
	// the store, the continuity handle, and the cycle cadence.
	continuity := &localAuditContinuity{}
	newAuditManagerConfig := func(anchorStore auditanchor.AnchorStore) auditanchor.ManagerConfig {
		return auditanchor.ManagerConfig{
			Store: anchorStore, Continuity: continuity,
			Interval: options.auditAnchorInterval, MaxAge: options.auditAnchorMaxAge,
		}
	}

	path := filepath.Join(t.TempDir(), "oberth.sqlite")
	database, err := openStartupDatabase(ctx, path, continuity, witnessChainReset{}, witnessGenesisAdoption{}, func(inspection *store.Store) error {
		verifier, managerErr := auditanchor.NewManager(newAuditManagerConfig(inspection))
		if managerErr != nil {
			return managerErr
		}
		return verifier.VerifyStartup(ctx)
	})
	if err != nil {
		t.Fatalf("fresh local-mode startup: %v", err)
	}
	defer func() { _ = database.Close() }()

	anchors, err := auditanchor.NewManager(newAuditManagerConfig(database))
	if err != nil {
		t.Fatal(err)
	}
	if err := anchors.Initialize(ctx); err != nil {
		t.Fatalf("local-mode initialization: %v", err)
	}
	if err := anchors.Ready(ctx); err != nil {
		t.Fatalf("local-mode readiness: %v", err)
	}
	if err := anchors.AllowMutation(ctx); err != nil {
		t.Fatalf("local-mode mutation gate: %v", err)
	}
	if _, err := database.AppendAuditAction(ctx, model.AuditActionSpec{
		Actor: "operator", Action: "upstream.register", ResourceType: "upstream", ResourceID: "codeberg",
	}); err != nil {
		t.Fatal(err)
	}
	if err := anchors.AllowMutation(ctx); err != nil {
		t.Fatalf("local-mode mutation gate after append: %v", err)
	}
	head, err := database.VerifyAuditState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if head.ID != 1 {
		t.Fatalf("local chain head = %d, want 1", head.ID)
	}
	// The genesis emptiness proof reads intents and pins once each; nothing
	// else may touch rollback-external witness state in local mode.
	if continuity.prepareCalls != 0 || continuity.reconcileCalls != 0 {
		t.Fatalf("local mode touched external witness state: %+v", continuity)
	}

	// No checkpoint may exist to go stale: readiness in local mode never
	// depends on an anchor age or a periodic external refresh.
	if anchor, err := database.LatestAuditAnchor(ctx); err == nil {
		t.Fatalf("local mode recorded a signed checkpoint: %+v", anchor)
	}
	if err := anchors.Ready(ctx); err != nil {
		t.Fatalf("local-mode readiness after mutations: %v", err)
	}
}
