package service_test

import (
	"strings"
	"sync"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/identityname"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestRepositoryRemovalRaceExplainsCompletedRevocation(t *testing.T) {
	t.Parallel()
	p := people(t)
	f := attachRevokeStore(t, p)
	q := secretTarget()
	approveSecret(t, p, q)
	for _, kind := range []identityname.Kind{identityname.StepCI, identityname.Step} {
		seedSecret(t, f, secretAccountFor(t, q, kind), q.Secret)
	}
	var issue model.Issue
	f.before = sync.OnceFunc(func() {
		var err error
		issue, err = p.state.CreateIssue(t.Context(), stateport.Action{Actor: admin().Identity}, model.Issue{
			Kind: model.IssueManual, Title: "History arriving during cleanup", Repo: repository, Upstream: "origin",
		})
		if err != nil {
			t.Fatal(err)
		}
	})
	err := removeSecretRepository(t, p)
	if err == nil {
		t.Fatal("removal crossed newly retained history")
	}
	refusal := publicerr.Classify(err)
	if refusal.Code() != publicerr.Conflict || refusal.Reason() != "repository still owns durable history or storage" || !strings.Contains(refusal.Remedy(), "Secret grants were already revoked") {
		t.Fatal("removal race hid completed revocation", err)
	}
	if !strings.Contains(refusal.Detail, "history and storage ownership") {
		t.Fatal("removal race lost the store's recovery advice", err)
	}
	assertRemovalRaceCleanup(t, p, f, q)
	if _, err := p.state.Repository(t.Context(), repository, nil); err != nil {
		t.Fatal("removal lost the repository", err)
	}
	if _, err := p.state.Issue(t.Context(), issue.ID); err != nil {
		t.Fatal("removal lost arriving history", err)
	}
}

func assertRemovalRaceCleanup(t *testing.T, p *peopleFixture, f *revokeStore, q apicontract.SecretPutArgs) {
	t.Helper()
	assertGrantState(t, p, q, true)
	assertRemovalRevokes(t, p, 1)
	if deleteCalls(f) != 2 || len(deleteAudit(t, p)) != 2 {
		t.Fatal("cleanup did not delete and audit both tiers")
	}
	for _, kind := range []identityname.Kind{identityname.StepCI, identityname.Step} {
		if f.backend.Resources[storedPath(secretAccountFor(t, q, kind), q.Secret)] != nil {
			t.Fatal("cleanup retained a secret value")
		}
	}
}
