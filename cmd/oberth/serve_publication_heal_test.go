package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/auditanchor"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/storetest"
)

func seedLegacyPublicationRetry(t *testing.T, path string) (model.Publication, model.AuditHead) {
	t.Helper()
	ctx := t.Context()
	s, err := store.CreateGenesis(ctx, path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	u, err := s.RegisterUpstream(ctx, "fixture", model.UpstreamSpec{Name: "origin", Kind: "ssh", BaseURL: "ssh://git@example.invalid/org", KeyName: "key"})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := s.CreateRepository(ctx, model.RepositorySpec{Name: "repo", UpstreamID: u.ID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := storetest.EnqueueRun(s, ctx, model.RunSpec{RepoID: repo.ID, RefKind: model.RefBranch, Ref: "feature/retry", SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Actor: "actor", Trigger: "branch"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = storetest.ClaimNextRun(s, ctx); err != nil {
		t.Fatal(err)
	}
	pub, err := s.BeginPublication(ctx, model.PublicationSpec{RepoID: repo.ID, RunID: run.ID, RefKind: run.RefKind, Ref: run.Ref, ResultSHA: run.SHA, Actor: run.Actor})
	if err != nil {
		t.Fatal(err)
	}
	details, err := json.Marshal(map[string]string{"repo_id": fmt.Sprint(pub.RepoID), "ref_kind": string(pub.RefKind), "ref": pub.Ref, "previous_sha": "", "previous_known": "false", "result_sha": pub.ResultSHA, "run_id": pub.RunID, "promotion_id": "", "error": "retrying failed publication " + pub.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AppendAuditAction(ctx, model.AuditActionSpec{Actor: pub.Actor, Action: "publication.retry", ResourceType: "publication", ResourceID: pub.ID, Details: string(details)}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.VerifyAuditState(ctx); err == nil {
		t.Fatal("fixture did not break strict gate")
	}
	head, err := s.VerifyAuditChain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return pub, head
}
func TestProductionStartupHealsLegacyPublicationBeforeFirstGate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oberth.sqlite")
	pub, before := seedLegacyPublicationRetry(t, path)
	database, err := openStartupDatabase(t.Context(), path, &staticStartupContinuity{}, witnessChainReset{}, witnessGenesisAdoption{}, func(inspection *store.Store) error {
		verifier, err := auditanchor.NewManager(startupPublicationConfig(auditanchor.ManagerConfig{Store: inspection, Interval: time.Minute, MaxAge: time.Hour}))
		if err != nil {
			return err
		}
		return verifier.VerifyStartup(t.Context())
	})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err = database.RecoverOwnerState(t.Context()); err != nil {
		t.Fatal(err)
	}
	anchors, err := auditanchor.NewManager(auditanchor.ManagerConfig{Store: database, Interval: time.Minute, MaxAge: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if err = anchors.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = anchors.AllowMutation(t.Context()); err != nil {
		t.Fatal(err)
	}
	after, err := database.VerifyAuditState(context.Background())
	if err != nil || after.ID != before.ID+1 {
		t.Fatalf("heal audit head: %+v %v", after, err)
	}
	pending, err := database.PendingPublications(t.Context())
	if err != nil || len(pending) != 1 || pending[0].ID != pub.ID {
		t.Fatalf("pending state: %+v %v", pending, err)
	}
}

func TestStartupRepairWaitsForExternalVerification(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oberth.sqlite")
	_, before := seedLegacyPublicationRetry(t, path)
	blocked := fmt.Errorf("external continuity unavailable")
	database, err := openStartupDatabase(t.Context(), path, &staticStartupContinuity{}, witnessChainReset{}, witnessGenesisAdoption{}, func(s *store.Store) error {
		if _, err := s.VerifyStartupAuditState(t.Context()); err != nil {
			return err
		}
		return blocked
	})
	if database != nil {
		database.Close()
	}
	if !errors.Is(err, blocked) {
		t.Fatalf("verification failure: %v", err)
	}
	database, err = store.OpenCurrent(t.Context(), path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	after, err := database.VerifyAuditChain(t.Context())
	if err != nil || after.ID != before.ID || !bytes.Equal(after.SHA256, before.SHA256) {
		t.Fatalf("startup wrote before continuity verification: %+v %v", after, err)
	}
	if _, err = database.VerifyAuditState(t.Context()); err == nil {
		t.Fatal("runtime gate accepted unrepaired retry")
	}
}
func TestStartupRepairRejectsOtherAuditActions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oberth.sqlite")
	pub, _ := seedLegacyPublicationRetry(t, path)
	database, err := store.OpenCurrent(t.Context(), path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = database.AppendAuditAction(t.Context(), model.AuditActionSpec{Actor: pub.Actor, Action: "publication.failed", ResourceType: "publication", ResourceID: pub.ID, Details: "{}"}); err != nil {
		t.Fatal(err)
	}
	before, err := database.VerifyAuditChain(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = database.ReconcileInconsistentPublications(t.Context()); err == nil {
		t.Fatal("repair accepted unrelated publication corruption")
	}
	after, err := database.VerifyAuditChain(t.Context())
	if err != nil || after.ID != before.ID {
		t.Fatalf("rejected repair appended an audit action: %+v %v", after, err)
	}
	database.Close()
}
