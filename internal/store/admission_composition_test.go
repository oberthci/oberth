package store

import (
	"context"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/identityname"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/workflowpolicy"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func TestRealAdmissionRecordsAfterGateAuthorityAndArchiveChange(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	repo := atomicRepository(t, s)
	key := stateport.Key{Fingerprint: "admission-key", Identity: "agent", Kind: "agent", ParkOnPush: true, Scope: []string{"origin"}}
	requireOK(t, s.RegisterKey(t.Context(), stateport.Action{Actor: "operator"}, key))
	grant := workflowpolicy.GrantKey{Key: identityname.Key{Repository: repo.Canonical, File: ".oberth/build.yaml", Step: "main"}, Path: "build/token"}
	requireOK(t, s.SecretGrant(t.Context(), stateport.Action{Actor: "operator"}, grant, false))
	filter := stateport.CatalogFilter{Repositories: []string{repo.Canonical}, Limit: 200}
	grants, err := s.SecretGrants(t.Context(), filter)
	requireOK(t, err)
	if len(grants) != 1 || grants[0].Revoked {
		t.Fatal("preparation grant was not active")
	}
	r := admissionRequest(t, repo.Canonical)
	r.Grants = []admissioncontract.GrantEvidence{{Repository: repo.Canonical, File: grant.File, Step: grant.Step, Kind: "secret", Path: grant.Path, State: admissioncontract.StateVerified}}
	p, err := argoworkflow.Prepare(t.Context(), r, argoworkflow.Policy{}, compositionIdentities())
	requireOK(t, err)
	who := actor.Actor{Identity: key.Identity, AuthMethod: "cert", Fingerprint: key.Fingerprint, Scope: key.Scope}
	req := model.CIRequest{ReceiveID: "sealed-push", Repo: r.Repository, Ref: r.Ref, ObjectSHA: r.ObjectSHA, CommitSHA: new(r.CommitSHA), Trigger: r.Trigger, Actor: who}
	batch := stateport.BatchAcceptance{ReceiveID: req.ReceiveID, Repository: req.Repo, Actor: who, Runs: []stateport.Acceptance{{Request: req, Prepared: p}}}
	requireOK(t, s.SetKeyScope(t.Context(), stateport.Action{Actor: "operator"}, key.Fingerprint, []string{"foreign"}))
	requireOK(t, s.RevokeKey(t.Context(), stateport.Action{Actor: "operator"}, key.Fingerprint))
	requireOK(t, s.SecretGrant(t.Context(), stateport.Action{Actor: "operator"}, grant, true))
	requireOK(t, s.ArchiveRepository(t.Context(), stateport.Action{Actor: "operator"}, repo.ID))
	grants, err = s.SecretGrants(t.Context(), filter)
	requireOK(t, err)
	current, err := s.Key(t.Context(), key.Fingerprint)
	requireOK(t, err)
	if len(grants) != 1 || !grants[0].Revoked || !current.Revoked || current.Scope[0] != "foreign" {
		t.Fatal("authority-change control did not take effect")
	}
	checkCompositionReceipt(t, s, batch)
}

func checkCompositionReceipt(t *testing.T, s *Store, batch stateport.BatchAcceptance) {
	t.Helper()
	p := batch.Runs[0].Prepared
	before, err := p.MarshalJSON()
	requireOK(t, err)
	recorded, err := s.AcceptBatch(t.Context(), batch)
	requireOK(t, err)
	if !recorded.Fresh || len(recorded.Runs) != 1 {
		t.Fatal("sealed decision did not record", recorded)
	}
	stored, err := s.Run(t.Context(), p.View().RunID)
	requireOK(t, err)
	if stored.Admission.Digest != p.View().Digest || stored.Total != len(p.View().Plan.Entries) || encode(stored.Admission) != encode(p.View()) {
		t.Fatal("admission was reconstructed", stored)
	}
	after, err := p.MarshalJSON()
	requireOK(t, err)
	if string(before) != string(after) {
		t.Fatal("recording changed the seal")
	}
	replay, err := s.AcceptBatch(t.Context(), batch)
	requireOK(t, err)
	if replay.Fresh || len(replay.Runs) != 1 {
		t.Fatal("sealed receipt replay changed after revocation")
	}
}

func admissionRequest(t *testing.T, repo string) admissioncontract.PrepareRequest {
	t.Helper()
	doc := []byte(`{"apiVersion":"argoproj.io/v1alpha1","kind":"Workflow","spec":{"entrypoint":"main","templates":[{"name":"main","container":{"image":"oberth.ci/test:1","command":["true"]}}]}}`)
	sha := strings.Repeat("a", 40)
	blob := strings.Repeat("b", 40)
	file := ".oberth/build.yaml"
	return admissioncontract.PrepareRequest{Mode: admissioncontract.ModeSubmit, Repository: repo, RunID: "sealed-run", Ref: "refs/heads/main", ObjectSHA: sha, CommitSHA: sha, Trigger: model.TriggerBranch, SelectedFile: file, FileBlob: blob, Document: doc,
		SourceReceipt:   sourcecontract.Receipt{Version: 1, Revision: sourcecontract.Revision{Repository: repo, Commit: sha}, ObjectSHA: sha, File: file, Blob: blob, DocumentDigest: sourcecontract.Digest(doc)},
		IdentityRequest: admissioncontract.IdentityRequest{Mode: admissioncontract.ModeSubmit, Namespace: "ci", PipelineAccount: "ci-step"}}
}

func compositionIdentities() admissioncontract.PrepareHooks {
	return admissioncontract.PrepareHooks{VerifyIdentities: func(_ context.Context, r admissioncontract.IdentityRequest) (admissioncontract.IdentityEvidence, error) {
		return admissioncontract.IdentityEvidence{Namespace: r.Namespace, State: admissioncontract.StateVerified, Pipeline: admissioncontract.AccountEvidence{Name: r.PipelineAccount, UID: "step-uid"}}, nil
	}}
}
