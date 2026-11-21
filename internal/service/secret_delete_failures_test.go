package service_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/identityname"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestSecretRevokeFailureStaysRevokedAndRepeatsDelete(t *testing.T) {
	t.Parallel()
	for _, failure := range []string{"sealed", "unreachable", "role-refused", "audit", "unconfigured"} {
		t.Run(failure, func(t *testing.T) { checkSecretRevokeFailure(t, failure) })
	}
}

func checkSecretRevokeFailure(t *testing.T, failure string) {
	t.Helper()
	p := people(t)
	f := attachRevokeStore(t, p)
	q := secretTarget()
	approveSecret(t, p, q)
	account := secretAccountFor(t, q, identityname.StepCI)
	seedSecret(t, f, account, q.Secret)
	setRevokeFailure(t, p, f, failure)
	result, err := revokeSecret(t, p, q)
	if err == nil {
		t.Fatal("failed delete reported success")
	}
	refusal := publicerr.Classify(err)
	if !strings.Contains(refusal.Reason(), "revoked") || !strings.Contains(refusal.Reason(), "still stored") || !strings.Contains(refusal.Remedy(), "repeat the revoke") {
		t.Fatal("partial revocation was not explained", err)
	}
	assertRevokeRemedy(t, failure, err)
	assertGrantState(t, p, q, true)
	assertRevocationPublished(t, p, q)
	if f.backend.Resources[storedPath(account, q.Secret)] == nil {
		t.Fatal("failed delete removed the fixture")
	}
	if failure == "audit" || failure == "unconfigured" {
		assertNoDeleteRequests(t, p, f)
	}
	assertDeletePublic(t, p, result, err)
	f.backend.Sealed = false
	f.unreachable = false
	f.refused = false
	p.admin.Fail = nil
	p.f.c.People.SecretWriter = f.writer
	p.f.restart(t)
	result, err = revokeSecret(t, p, q)
	if err != nil || result.State != "revoked" || f.backend.Resources[storedPath(account, q.Secret)] != nil {
		t.Fatal("repeat did not delete the retained value", result, err)
	}
	assertGrantState(t, p, q, true)
	assertDeletePublic(t, p, result, err)
}

func TestSecretRevokeAbsentValueIsDeleted(t *testing.T) {
	t.Parallel()
	p := people(t)
	f := attachRevokeStore(t, p)
	q := secretTarget()
	approveSecret(t, p, q)
	f.absent = true
	for range 2 {
		result, err := revokeSecret(t, p, q)
		if err != nil || result.State != "revoked" {
			t.Fatal("absent value was not treated as deleted", result, err)
		}
	}
	if deleteCalls(f) != 4 || len(deleteAudit(t, p)) != 4 {
		t.Fatal("already-revoked grant skipped delete")
	}
	assertGrantState(t, p, q, true)
}

func TestSecretRevokeRequiresAdministrator(t *testing.T) {
	t.Parallel()
	p := people(t)
	f := attachRevokeStore(t, p)
	q := secretTarget()
	approveSecret(t, p, q)
	call, err := p.f.s.PrepareAccessRevoke(t.Context(), workerCaller(), apicontract.AccessRevokeArgs{Repo: q.Repo, Workflow: q.Workflow, Step: q.Step, Secret: q.Secret})
	if err == nil || call != nil {
		t.Fatal("non-administrator could revoke")
	}
	assertGrantState(t, p, q, false)
	if deleteCalls(f) != 0 || len(deleteAudit(t, p)) != 0 {
		t.Fatal("unauthorized revoke touched the value")
	}
}

func TestNonStoreGrantRevocationNeverDeletes(t *testing.T) {
	t.Parallel()
	for _, kind := range []stateport.GrantKind{stateport.GrantSecret, stateport.GrantConfigMap, stateport.GrantImagePull, stateport.GrantAudience, stateport.GrantPVC} {
		t.Run(string(kind), func(t *testing.T) { checkNonStoreRevoke(t, kind) })
	}
}

func checkNonStoreRevoke(t *testing.T, kind stateport.GrantKind) {
	t.Helper()
	p := people(t)
	f := attachRevokeStore(t, p)
	q := secretTarget()
	account := secretAccountFor(t, q, identityname.StepCI)
	seedSecret(t, f, account, q.Secret)
	request := apicontract.AccessRevokeArgs{Repo: q.Repo, Workflow: q.Workflow, Step: q.Step, Secret: q.Secret, Kind: string(kind)}
	if kind == stateport.GrantAudience {
		request.Secret, request.Audience = "identity", "publisher"
	}
	if kind == stateport.GrantPVC {
		request.Workflow = ""
		request.Step = ""
		request.Secret = ""
		request.Claim = "cache"
		request.Tier = "ci"
	}
	call, err := p.f.s.PrepareAccessRevoke(t.Context(), adminCaller(), request)
	result, err := execute(t.Context(), call, err)
	if err != nil || result.State != "revoked" {
		t.Fatal("non-secret revoke changed", result, err)
	}
	if deleteCalls(f) != 0 || len(deleteAudit(t, p)) != 0 || f.backend.Resources[storedPath(account, q.Secret)] == nil {
		t.Fatal("non-secret revoke deleted a stored value")
	}
}

func setRevokeFailure(t *testing.T, p *peopleFixture, f *revokeStore, failure string) {
	t.Helper()
	if failure == "sealed" {
		f.backend.Sealed = true
	}
	if failure == "unreachable" {
		f.unreachable = true
	}
	if failure == "role-refused" {
		f.refused = true
	}
	if failure == "unconfigured" {
		p.f.c.People.SecretWriter = nil
	}
	if failure == "audit" {
		p.admin.Fail = errors.New("private-audit-failure")
		p.f.c.People.Admin = p.admin
	}
	p.f.restart(t)
}

func assertRevokeRemedy(t *testing.T, failure string, err error) {
	t.Helper()
	public := publicerr.Classify(err)
	expected := map[string]string{"sealed": "sealed", "unreachable": "connection failed", "role-refused": "refused", "unconfigured": "not configured"}
	if text, ok := expected[failure]; ok && !strings.Contains(strings.ToLower(public.Detail), text) {
		t.Fatal("store failure detail missing", err)
	}
	if failure == "sealed" && !strings.Contains(public.Detail, "unseal") {
		t.Fatal("sealed repair missing", err)
	}
	if failure == "role-refused" && !strings.Contains(public.Detail, "role") {
		t.Fatal("role repair missing", err)
	}
	if failure != "sealed" && strings.Contains(public.Remedy(), "unseal") {
		t.Fatal("unseal prescribed for another failure", err)
	}
}

func assertNoDeleteRequests(t *testing.T, p *peopleFixture, f *revokeStore) {
	t.Helper()
	if deleteCalls(f) != 0 || len(deleteAudit(t, p)) != 0 {
		t.Fatal("delete crossed an unavailable writer or audit")
	}
}
