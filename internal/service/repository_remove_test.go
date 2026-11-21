package service_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/identityname"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestRepositoryRemovalRefusalKeepsSecretGrantAndValues(t *testing.T) {
	p := people(t)
	f := attachRevokeStore(t, p)
	q := secretTarget()
	approveSecret(t, p, q)
	for _, kind := range []identityname.Kind{identityname.StepCI, identityname.Step} {
		seedSecret(t, f, secretAccountFor(t, q, kind), q.Secret)
	}
	_, err := p.state.CreateIssue(t.Context(), stateport.Action{Actor: admin().Identity}, model.Issue{
		Kind: model.IssueManual, Title: "Retained history", Repo: repository, Upstream: "origin",
	})
	if err != nil {
		t.Fatal(err)
	}
	err = removeSecretRepository(t, p)
	if err == nil || publicerr.Classify(err).Reason() != "repository still owns durable history or storage" {
		t.Fatal("removal must refuse retained history", err)
	}
	if strings.Contains(publicerr.Classify(err).Remedy(), "already revoked") {
		t.Fatal("preflight refusal claimed cleanup occurred", err)
	}
	assertGrantState(t, p, q, false)
	if deleteCalls(f) != 0 || len(deleteAudit(t, p)) != 0 {
		t.Fatal("refused removal attempted secret cleanup")
	}
	assertRemovalRevokes(t, p, 0)
	for _, kind := range []identityname.Kind{identityname.StepCI, identityname.Step} {
		value := f.backend.Resources[storedPath(secretAccountFor(t, q, kind), q.Secret)]
		if value == nil || value["value"] != base64.StdEncoding.EncodeToString([]byte("private-delete-value")) {
			t.Fatal("refused removal changed a secret value")
		}
	}
	if _, err := p.state.Repository(t.Context(), repository, nil); err != nil {
		t.Fatal("refused removal lost repository", err)
	}
}

func TestRepositoryRemovalRevokesAndDeletesEverySecret(t *testing.T) {
	p := people(t)
	f := attachRevokeStore(t, p)
	q := secretTarget()
	for _, path := range []string{"old/one", "old/two"} {
		q.Secret = path
		approveSecret(t, p, q)
		for _, kind := range []identityname.Kind{identityname.StepCI, identityname.Step} {
			seedSecret(t, f, secretAccountFor(t, q, kind), path)
		}
	}
	if err := removeSecretRepository(t, p); err != nil {
		t.Fatal(err)
	}
	if deleteCalls(f) != 4 || len(deleteAudit(t, p)) != 4 {
		t.Fatal("removal skipped a tier's value or audit")
	}
	assertRemovalRevokes(t, p, 2)
	for path := range f.backend.Resources {
		if strings.Contains(path, "/steps/") {
			t.Fatal("repository secret retained", path)
		}
	}
	if _, err := p.state.Repository(t.Context(), repository, nil); err == nil {
		t.Fatal("repository remains")
	}
	assertNewRepositorySecretScope(t, p, f)
}

func assertNewRepositorySecretScope(t *testing.T, p *peopleFixture, f *revokeStore) {
	t.Helper()
	q := secretTarget()
	next, err := p.state.RegisterRepository(t.Context(), stateport.Action{Actor: admin().Identity}, repository)
	if err != nil {
		t.Fatal(err)
	}
	p.f.store.native.SeedRepository(next)
	q.Secret = "new/only"
	approveSecret(t, p, q)
	for _, kind := range []identityname.Kind{identityname.StepCI, identityname.Step} {
		account := secretAccountFor(t, q, kind)
		seedSecret(t, f, account, q.Secret)
		for _, old := range []string{"old/one", "old/two"} {
			f.assertRevokedValueCannotBeRead(t, account, old, q.Secret)
		}
	}
	snapshot, err := p.state.Grants(t.Context(), []string{repository})
	if err != nil || len(snapshot.Grants) != 1 || snapshot.Grants[0].Path != q.Secret {
		t.Fatal("re-registration revived authority", snapshot, err)
	}
}

func assertRemovalRevokes(t *testing.T, p *peopleFixture, want int) {
	t.Helper()
	rows, err := p.state.AdministrativeAudit(t.Context(), nil, stateport.AdministrativeFilter{Action: "authority.changed", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, row := range rows {
		var detail struct{ Grant stateport.GrantChange }
		if json.Unmarshal([]byte(row.Details), &detail) != nil {
			t.Fatal("invalid grant audit")
		}
		if detail.Grant.Kind == stateport.GrantStorePath && detail.Grant.Revoke {
			count++
		}
	}
	if count != want {
		t.Fatal("missing per-grant revoke audit", count)
	}
}

func TestRepositoryRemovalKeepsRepositoryUntilSecretCleanupSucceeds(t *testing.T) {
	for _, failure := range []string{"sealed", "audit", "unconfigured", "publication", "second-tier"} {
		t.Run(failure, func(t *testing.T) { checkRemovalCleanupFailure(t, failure) })
	}
}

func checkRemovalCleanupFailure(t *testing.T, failure string) {
	t.Helper()
	p := people(t)
	f := attachRevokeStore(t, p)
	q := secretTarget()
	approveSecret(t, p, q)
	seedSecret(t, f, secretAccountFor(t, q, identityname.StepCI), q.Secret)
	seedSecret(t, f, secretAccountFor(t, q, identityname.Step), q.Secret)
	setRevokeFailure(t, p, f, failure)
	if failure == "second-tier" {
		f.backend.Fail = storedPath(secretAccountFor(t, q, identityname.Step), q.Secret)
	}
	if failure == "publication" {
		p.publish.Fence = func(context.Context, func(context.Context) (stateport.AuthoritySnapshot, error)) error {
			return errors.New("publication unavailable")
		}
	}
	err := removeSecretRepository(t, p)
	if err == nil {
		t.Fatal("incomplete cleanup removed repository")
	}
	if failure != "publication" && !strings.Contains(publicerr.Classify(err).Remedy(), "repeat the revoke") {
		t.Fatal("cleanup recovery missing", err)
	}
	if _, err := p.state.Repository(t.Context(), repository, nil); err != nil {
		t.Fatal("failed cleanup removed repository", err)
	}
	assertGrantState(t, p, q, true)
	f.backend.Sealed, p.admin.Fail, p.publish.Fence = false, nil, nil
	f.backend.Fail = ""
	p.f.c.People.SecretWriter = f.writer
	p.f.restart(t)
	if err := removeSecretRepository(t, p); err != nil {
		t.Fatal("retry skipped previously revoked value", err)
	}
	for _, kind := range []identityname.Kind{identityname.StepCI, identityname.Step} {
		if f.backend.Resources[storedPath(secretAccountFor(t, q, kind), q.Secret)] != nil {
			t.Fatal("retry retained the value")
		}
	}
}

func removeSecretRepository(t *testing.T, p *peopleFixture) error {
	t.Helper()
	call, err := p.f.s.PrepareRepoRemove(t.Context(), adminCaller(), apicontract.RepoRemoveArgs{Repo: repository, Reason: "Remove repository authority"})
	_, err = execute(t.Context(), call, err)
	return err
}
