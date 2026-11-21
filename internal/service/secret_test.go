package service_test

import (
	"bytes"
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
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/workflowpolicy"
)

type secretWriterFunc func(context.Context, string, string, []byte) error

func (f secretWriterFunc) Put(ctx context.Context, account, path string, value []byte) error {
	return f(ctx, account, path, value)
}

func (f secretWriterFunc) Delete(context.Context, string, string) error {
	return errors.New("unexpected delete")
}

func secretTarget() apicontract.SecretPutArgs {
	return apicontract.SecretPutArgs{Repo: repository, Workflow: ".oberth/build.yaml", Step: "build", Secret: "registry/key"}
}
func approveSecret(t *testing.T, p *peopleFixture, q apicontract.SecretPutArgs) {
	t.Helper()
	_, err := p.state.ChangeGrant(t.Context(), stateport.Action{Actor: admin().Identity}, stateport.GrantChange{Kind: stateport.GrantStorePath, Repository: q.Repo, File: q.Workflow, Step: q.Step, Path: q.Secret})
	if err != nil {
		t.Fatal(err)
	}
}
func putSecret(t *testing.T, p *peopleFixture, q apicontract.SecretPutArgs, value []byte) (apicontract.Receipt, error) {
	t.Helper()
	ctx := apicontract.WithSecretValue(t.Context(), value)
	call, err := p.f.s.PrepareSecretPut(ctx, adminCaller(), q)
	return execute(ctx, call, err)
}

func TestSecretValueNeverEntersAuditEventsOrResult(t *testing.T) {
	t.Parallel()
	p := people(t)
	q := secretTarget()
	approveSecret(t, p, q)
	value := []byte("private-body-marker")
	p.f.c.People.SecretWriter = secretWriterFunc(func(ctx context.Context, account, path string, got []byte) error {
		rows, err := p.state.AdministrativeAudit(ctx, nil, stateport.AdministrativeFilter{Action: "secret.put.requested", Limit: 10})
		if err != nil || len(rows) != 1 {
			t.Fatal("audit must commit before write", err)
		}
		raw, _ := json.Marshal(rows)
		assertSecretAbsent(t, raw, value)
		if !bytes.Equal(got, value) || path != q.Secret {
			t.Fatal("private audit or changed value")
		}
		want, _ := identityname.Name(identityname.Key{Repository: q.Repo, File: q.Workflow, Step: q.Step}, identityname.StepCI)
		if account != want {
			t.Fatal("wrong account")
		}
		return nil
	})
	p.f.restart(t)
	result, err := putSecret(t, p, q, value)
	if err != nil || result.State != "stored" {
		t.Fatal(result, err)
	}
	raw, _ := json.Marshal(result)
	assertSecretAbsent(t, raw, value)
	events, err := p.state.Events(t.Context(), stateport.EventFilter{Repositories: []string{repository}, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(events)
	assertSecretAbsent(t, raw, value)
}

func TestSecretWriterRefusesAnotherRepositoryAndUnapprovedTuple(t *testing.T) {
	t.Parallel()
	for _, field := range []string{"repo", "workflow", "step", "secret", "revoked"} {
		t.Run(field, func(t *testing.T) {
			p := people(t)
			q := secretTarget()
			approveSecret(t, p, q)
			p.f.c.People.SecretWriter = secretWriterFunc(func(context.Context, string, string, []byte) error { t.Fatal("unapproved write"); return nil })
			p.f.restart(t)
			switch field {
			case "repo":
				q.Repo = "origin/team/other"
				p.f.store.native.SeedRepository(stateport.Repository{ID: 2, Upstream: "origin", Name: "other", Canonical: q.Repo})
			case "workflow":
				q.Workflow = ".oberth/other.yaml"
			case "step":
				q.Step = "other"
			case "secret":
				q.Secret = "other/path"
			case "revoked":
				_, err := p.state.ChangeGrant(t.Context(), stateport.Action{Actor: admin().Identity}, stateport.GrantChange{Kind: stateport.GrantStorePath, Repository: q.Repo, File: q.Workflow, Step: q.Step, Path: q.Secret, Revoke: true})
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := putSecret(t, p, q, []byte("private")); err == nil {
				t.Fatal("unapproved tuple accepted")
			}
		})
	}
}

func TestSecretWriterBoundsBodyAuthorityAndAuditFailure(t *testing.T) {
	t.Parallel()
	p := people(t)
	q := secretTarget()
	approveSecret(t, p, q)
	calls := 0
	p.f.c.People.SecretWriter = secretWriterFunc(func(context.Context, string, string, []byte) error { calls++; return nil })
	p.f.restart(t)
	if _, err := putSecret(t, p, q, make([]byte, model.MaxSecretValueBytes)); err != nil {
		t.Fatal("exact bound refused", err)
	}
	if _, err := putSecret(t, p, q, make([]byte, model.MaxSecretValueBytes+1)); err == nil {
		t.Fatal("oversize accepted")
	}
	if _, err := p.f.s.PrepareSecretPut(t.Context(), adminCaller(), q); err == nil {
		t.Fatal("body absent")
	}
	ctx := apicontract.WithSecretValue(t.Context(), []byte("private"))
	if _, err := p.f.s.PrepareSecretPut(ctx, workerCaller(), q); err == nil {
		t.Fatal("non-admin accepted")
	}
	p.admin.Fail = errors.New("audit unavailable")
	p.f.c.People.Admin = p.admin
	p.f.restart(t)
	if _, err := putSecret(t, p, q, []byte("private")); err == nil || calls != 1 {
		t.Fatal("write crossed audit failure", calls, err)
	}
}

func TestSecretWriterSanitizesDependencyErrorsAndSeparatesReleaseAccount(t *testing.T) {
	t.Parallel()
	for _, sealed := range []bool{false, true} {
		t.Run(map[bool]string{false: "private-error", true: "sealed"}[sealed], func(t *testing.T) {
			p := people(t)
			q := secretTarget()
			q.Workflow = ".oberth/publish.yaml"
			approveSecret(t, p, q)
			p.f.c.People.ReleaseRoutes = workflowpolicy.ReleaseRoutes{repository: {{Pattern: "v*", File: q.Workflow}}}
			p.f.c.People.SecretWriter = secretWriterFunc(func(_ context.Context, account, _ string, _ []byte) error {
				want, _ := identityname.Name(identityname.Key{Repository: q.Repo, File: q.Workflow, Step: q.Step}, identityname.Step)
				if account != want {
					t.Fatal("release account entered CI subtree")
				}
				if sealed {
					return publicerr.New(publicerr.Unavailable, "Store maintenance is required.", "An administrator runs oberth unseal, then retry.", errors.New("private-body-marker"))
				}
				return errors.New("private-body-marker")
			})
			p.f.restart(t)
			_, err := putSecret(t, p, q, []byte("private-body-marker"))
			if err == nil || strings.Contains(err.Error(), "private-body-marker") {
				t.Fatal("private error escaped")
			}
			if sealed && (publicerr.Classify(err).Code() != publicerr.Unavailable || publicerr.Classify(err).Reason() != "Store maintenance is required." || !strings.Contains(err.Error(), "An administrator runs oberth unseal")) {
				t.Fatal("sealed remedy missing")
			}
		})
	}
}

var _ service.SecretWriter = secretWriterFunc(nil)

func assertSecretAbsent(t *testing.T, observation, value []byte) {
	t.Helper()
	encoded := []byte(base64.StdEncoding.EncodeToString(value))
	if bytes.Contains(observation, value) || bytes.Contains(observation, encoded) {
		t.Fatal("private value or its base64 encoding entered an observation")
	}
}

func TestSecretRevokedAfterPreparationIsNeverWritten(t *testing.T) {
	t.Parallel()
	p := people(t)
	target := secretTarget()
	approveSecret(t, p, target)
	p.f.c.People.SecretWriter = secretWriterFunc(func(context.Context, string, string, []byte) error {
		t.Fatal("revoked prepared call reached the store")
		return nil
	})
	p.f.restart(t)
	ctx := apicontract.WithSecretValue(t.Context(), []byte("private-body-marker"))
	call, err := p.f.s.PrepareSecretPut(ctx, adminCaller(), target)
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.state.ChangeGrant(ctx, stateport.Action{Actor: admin().Identity}, stateport.GrantChange{Kind: stateport.GrantStorePath, Repository: target.Repo, File: target.Workflow, Step: target.Step, Path: target.Secret, Revoke: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = call(ctx); err == nil || publicerr.Classify(err).Code() != publicerr.Forbidden {
		t.Fatal("revoked prepared call was accepted", err)
	}
	rows, err := p.state.AdministrativeAudit(ctx, nil, stateport.AdministrativeFilter{Action: "secret.put.requested", Limit: 10})
	if err != nil || len(rows) != 0 {
		t.Fatal("revoked call recorded write intent", err)
	}
}

func TestFailedSecretWriteLeavesOnlyRequestedAudit(t *testing.T) {
	t.Parallel()
	p := people(t)
	q := secretTarget()
	approveSecret(t, p, q)
	p.f.c.People.SecretWriter = secretWriterFunc(func(context.Context, string, string, []byte) error { return errors.New("private dependency failure") })
	p.f.restart(t)
	if _, err := putSecret(t, p, q, []byte("private-body-marker")); err == nil {
		t.Fatal("failed write reported stored")
	}
	rows, err := p.state.AdministrativeAudit(t.Context(), nil, stateport.AdministrativeFilter{Action: "secret.put.requested", Limit: 10})
	if err != nil || len(rows) != 1 {
		t.Fatal("write request audit missing", err)
	}
	rows, err = p.state.AdministrativeAudit(t.Context(), nil, stateport.AdministrativeFilter{Action: "secret.put", Limit: 10})
	if err != nil || len(rows) != 0 {
		t.Fatal("failed write claimed completed audit", err)
	}
}
