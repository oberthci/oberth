package service_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/identityname"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/workflowpolicy"
)

func TestSecretRevokeAfterReleaseRouteChange(t *testing.T) {
	t.Parallel()
	for _, wasRelease := range []bool{false, true} {
		t.Run(map[bool]string{false: "ci-to-release", true: "release-to-ci"}[wasRelease], func(t *testing.T) { checkRevokeRouteChange(t, wasRelease) })
	}
}

func checkRevokeRouteChange(t *testing.T, wasRelease bool) {
	t.Helper()
	p := people(t)
	f := attachRevokeStore(t, p)
	q := secretTarget()
	q.Workflow = ".oberth/custom.yaml"
	setSecretReleaseRoute(p, q, wasRelease)
	p.f.restart(t)
	approveSecret(t, p, q)
	result, err := putSecret(t, p, q, []byte("private-delete-value"))
	if err != nil || result.State != "stored" {
		t.Fatal(result, err)
	}
	kind := identityname.StepCI
	if wasRelease {
		kind = identityname.Step
	}
	original := storedPath(secretAccountFor(t, q, kind), q.Secret)
	if f.backend.Resources[original] == nil {
		t.Fatal("original account was not written")
	}
	setSecretReleaseRoute(p, q, !wasRelease)
	restartSecretDatabase(t, p)
	for range 2 {
		result, err = revokeSecret(t, p, q)
		if err != nil || result.State != "revoked" {
			t.Fatal(result, err)
		}
		if f.backend.Resources[original] != nil {
			t.Fatal("route change retained the original value")
		}
	}
	assertGrantState(t, p, q, true)
}

func setSecretReleaseRoute(p *peopleFixture, q apicontract.SecretPutArgs, release bool) {
	p.f.c.People.ReleaseRoutes = nil
	if release {
		p.f.c.People.ReleaseRoutes = workflowpolicy.ReleaseRoutes{q.Repo: {{Pattern: "v*", File: q.Workflow}}}
	}
}

func restartSecretDatabase(t *testing.T, p *peopleFixture) {
	t.Helper()
	if err := p.f.s.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := p.state.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := store.Open(t.Context(), p.path, store.Options{Clock: p.f.c.Now, Protocol: stateport.DefaultProtocol()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := next.Close(); err != nil {
			t.Error(err)
		}
	})
	p.state = next
	p.admin.Store = next
	c := &p.f.c.People
	c.StateMetadata = next
	c.Issues = next
	c.Enrollment = next
	c.Grants = next
	c.Admin = p.admin
	c.Backup = next
	p.f.restart(t)
}

func TestSecretDismissCannotEndAnApprovedOrRevokedGrant(t *testing.T) {
	t.Parallel()
	for _, revoked := range []bool{false, true} {
		t.Run(map[bool]string{false: "approved-after-preparation", true: "revoked-delete-pending"}[revoked], func(t *testing.T) { checkProtectedDismiss(t, revoked) })
	}
}

func TestSecretDismissPendingReturnsCommittedState(t *testing.T) {
	t.Parallel()
	for _, publishFails := range []bool{false, true} {
		t.Run(map[bool]string{false: "published", true: "incomplete-publication"}[publishFails], func(t *testing.T) {
			p := people(t)
			if publishFails {
				p.publish.Fence = func(context.Context, func(context.Context) (stateport.AuthoritySnapshot, error)) error {
					return errors.New("unrelated authority failure")
				}
			}
			q := secretTarget()
			call, err := p.f.s.PrepareAccessDismiss(t.Context(), adminCaller(), apicontract.AccessDismissArgs{Repo: q.Repo, Workflow: q.Workflow, Step: q.Step, Secret: q.Secret})
			result, err := execute(t.Context(), call, err)
			if err != nil || result.ID == "" || result.State != "dismissed" || (result.Warning != "") != publishFails {
				t.Fatal("committed dismissal lost its receipt", result, err)
			}
			snapshot, err := p.state.Grants(t.Context(), nil)
			if err != nil || len(snapshot.Grants) != 1 || !snapshot.Grants[0].Dismiss {
				t.Fatal("pending dismissal was not committed", snapshot, err)
			}
		})
	}
}

func checkProtectedDismiss(t *testing.T, revoked bool) {
	t.Helper()
	p := people(t)
	f := attachRevokeStore(t, p)
	q := secretTarget()
	call, err := p.f.s.PrepareAccessDismiss(t.Context(), adminCaller(), apicontract.AccessDismissArgs{Repo: q.Repo, Workflow: q.Workflow, Step: q.Step, Secret: q.Secret})
	if err != nil {
		t.Fatal(err)
	}
	approveSecret(t, p, q)
	if _, err := putSecret(t, p, q, []byte("private-delete-value")); err != nil {
		t.Fatal(err)
	}
	if revoked {
		f.backend.Sealed = true
		if _, err := revokeSecret(t, p, q); err == nil {
			t.Fatal("sealed fixture did not fail")
		}
	}
	result, err := execute(t.Context(), call, nil)
	assertDismissRefused(t, result, err)
	assertGrantState(t, p, q, revoked)
	list, e := p.f.s.PrepareAccessList(t.Context(), adminCaller(), apicontract.AccessListArgs{Repo: q.Repo, Revoked: true})
	page, e := execute(t.Context(), list, e)
	if e != nil || len(page.Grants) != 1 {
		t.Fatal("retry target disappeared", page, e)
	}
	if revoked {
		f.backend.Sealed = false
		if _, err := revokeSecret(t, p, q); err != nil {
			t.Fatal("retry target could not be deleted", err)
		}
	}
}

func assertDismissRefused(t *testing.T, result apicontract.Receipt, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("dismiss ended an approved or revoked grant")
	}
	if result != (apicontract.Receipt{}) {
		t.Fatal("refused dismissal returned a new state", result)
	}
	refusal := publicerr.Classify(err)
	if !strings.Contains(strings.ToLower(refusal.Reason()), "pending") || !strings.Contains(refusal.Remedy(), "access revoke") {
		t.Fatal("dismiss refusal lacks its repair", err)
	}
}

func assertRevocationPublished(t *testing.T, p *peopleFixture, q apicontract.SecretPutArgs) {
	t.Helper()
	for _, g := range p.publish.Snapshot.Grants {
		if g.Kind == stateport.GrantStorePath && g.Repository == q.Repo && g.File == q.Workflow && g.Step == q.Step && g.Path == q.Secret && g.Revoke {
			return
		}
	}
	t.Fatal("revocation was not published before the store operation")
}

func TestSecretDeleteFollowsFailedAuthorityPublication(t *testing.T) {
	t.Parallel()
	p := people(t)
	f := attachRevokeStore(t, p)
	q := secretTarget()
	approveSecret(t, p, q)
	seedSecret(t, f, secretAccountFor(t, q, identityname.StepCI), q.Secret)
	published := false
	p.publish.Fence = func(context.Context, func(context.Context) (stateport.AuthoritySnapshot, error)) error {
		published = true
		return errors.New("unrelated authority failure")
	}
	f.before = func() {
		if !published {
			t.Fatal("delete preceded authority publication")
		}
	}
	result, err := revokeSecret(t, p, q)
	if err != nil || !strings.Contains(result.Warning, "Retry the grant or revocation") {
		t.Fatal("authority warning lost", result, err)
	}
	if deleteCalls(f) != 2 {
		t.Fatal("authority failure skipped deletion")
	}
	assertGrantState(t, p, q, true)
}

func TestSecretRevokeRetainsFailureFromEitherAccount(t *testing.T) {
	t.Parallel()
	for _, kind := range []identityname.Kind{identityname.StepCI, identityname.Step} {
		t.Run(string(kind), func(t *testing.T) { checkDeleteAccountFailure(t, kind) })
	}
}

func checkDeleteAccountFailure(t *testing.T, failed identityname.Kind) {
	t.Helper()
	p := people(t)
	f := attachRevokeStore(t, p)
	q := secretTarget()
	approveSecret(t, p, q)
	for _, kind := range []identityname.Kind{identityname.StepCI, identityname.Step} {
		seedSecret(t, f, secretAccountFor(t, q, kind), q.Secret)
	}
	target := storedPath(secretAccountFor(t, q, failed), q.Secret)
	f.backend.Fail = target
	result, err := revokeSecret(t, p, q)
	if err == nil {
		t.Fatal("one failed account was reported as deleted")
	}
	if deleteCalls(f) != 2 || len(deleteAudit(t, p)) != 2 {
		t.Fatal("both account deletes were not attempted and audited")
	}
	if f.backend.Resources[target] == nil {
		t.Fatal("failed account fixture lost its value")
	}
	assertGrantState(t, p, q, true)
	assertRevocationPublished(t, p, q)
	assertDeletePublic(t, p, result, err)
	f.backend.Fail = ""
	result, err = revokeSecret(t, p, q)
	if err != nil || result.State != "revoked" {
		t.Fatal("retry failed", result, err)
	}
	for _, kind := range []identityname.Kind{identityname.StepCI, identityname.Step} {
		if f.backend.Resources[storedPath(secretAccountFor(t, q, kind), q.Secret)] != nil {
			t.Fatal("retry retained an account value")
		}
	}
}
