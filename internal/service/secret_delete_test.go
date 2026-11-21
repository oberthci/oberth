package service_test

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/identityname"
	"github.com/oberthci/oberth/internal/workflowpolicy"
)

func TestSecretRevokeDeletesOnlySelectedAccountPath(t *testing.T) {
	t.Parallel()
	for _, step := range []string{"build", "*"} {
		for _, release := range []bool{false, true} {
			t.Run(step+map[bool]string{false: "/ci", true: "/release"}[release], func(t *testing.T) { checkSecretRevokeAccount(t, step, release) })
		}
	}
}

func checkSecretRevokeAccount(t *testing.T, step string, release bool) {
	t.Helper()
	p := people(t)
	q := secretTarget()
	q.Step = step
	kind := identityname.StepCI
	if release {
		q.Workflow = ".oberth/release.yaml"
		kind = identityname.Step
		p.f.c.People.ReleaseRoutes = workflowpolicy.ReleaseRoutes{repository: {{Pattern: "v*", File: q.Workflow}}}
	}
	f := attachRevokeStore(t, p)
	approveSecret(t, p, q)
	keep := q
	keep.Secret = "retained/key"
	approveSecret(t, p, keep)
	account := secretAccountFor(t, q, kind)
	protected := seedProtectedSecretTargets(t, f, q)
	f.before = func() {
		assertGrantState(t, p, q, true)
		assertRevocationPublished(t, p, q)
		assertDeleteAuditSelectors(t, p, q, deleteCalls(f)+1)
	}
	result, err := revokeSecret(t, p, q)
	if err != nil || result.State != "revoked" {
		t.Fatal("revoke did not report deleted", result, err)
	}
	for _, targetKind := range []identityname.Kind{identityname.StepCI, identityname.Step} {
		if f.backend.Resources[storedPath(secretAccountFor(t, q, targetKind), q.Secret)] != nil {
			t.Fatal("revoked value retained")
		}
	}
	if deleteCalls(f) != 2 {
		t.Fatal("delete was skipped or repeated")
	}
	assertDeleteAuditAccounts(t, p, q, f)
	for _, path := range protected {
		if f.backend.Resources[path] == nil {
			t.Fatal("another secret was deleted")
		}
	}
	assertGrantState(t, p, keep, false)
	f.assertRevokedValueCannotBeRead(t, account, q.Secret, keep.Secret)
	assertDeletePublic(t, p, result, err)
}

func assertDeleteAuditSelectors(t *testing.T, p *peopleFixture, q apicontract.SecretPutArgs, expectedCount int) {
	t.Helper()
	rows := deleteAudit(t, p)
	if len(rows) != expectedCount {
		t.Fatal("delete request was not audited before dispatch")
	}
	var selectors map[string]string
	if json.Unmarshal([]byte(rows[0].Details), &selectors) != nil || len(selectors) != 5 {
		t.Fatal("delete audit has non-selector fields")
	}
	if selectors["tier"] != "ci" && selectors["tier"] != "release" {
		t.Fatal("delete audit does not identify its account tier")
	}
	raw, _ := json.Marshal(q)
	var expected map[string]string
	if json.Unmarshal(raw, &expected) != nil {
		t.Fatal("selector fixture invalid")
	}
	for key, value := range expected {
		if selectors[key] != value {
			t.Fatal("delete audit changed its selectors")
		}
	}
}

func assertDeleteAuditAccounts(t *testing.T, p *peopleFixture, q apicontract.SecretPutArgs, f *revokeStore) {
	t.Helper()
	rows := deleteAudit(t, p)
	i := len(rows) - 1
	for _, call := range f.backend.Calls {
		if call.Method != "DELETE" {
			continue
		}
		var selectors map[string]string
		if i < 0 || json.Unmarshal([]byte(rows[i].Details), &selectors) != nil {
			t.Fatal("delete request audit missing")
		}
		kind := identityname.Step
		if selectors["tier"] == "ci" {
			kind = identityname.StepCI
		}
		if call.Path != storedPath(secretAccountFor(t, q, kind), q.Secret) {
			t.Fatal("delete audit tier differs from the requested account")
		}
		i--
	}
}

func (f *revokeStore) assertRevokedValueCannotBeRead(t *testing.T, account, revoked, retained string) {
	t.Helper()
	claim := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"system:serviceaccount:ci:` + account + `"}`))
	token := []byte("header." + claim + ".signature")
	if values, err := f.writer.Client.Fetch(t.Context(), f.writer.Config, token, []string{revoked}); err == nil || values != nil {
		t.Fatal("account could read revoked secret")
	}
	values, err := f.writer.Client.Fetch(t.Context(), f.writer.Config, token, []string{retained})
	if err != nil || string(values[retained]) != "private-delete-value" {
		t.Fatal("remaining grant lost its stored value")
	}
}

func seedProtectedSecretTargets(t *testing.T, f *revokeStore, q apicontract.SecretPutArgs) []string {
	t.Helper()
	keep, other, foreign, counterpart, sibling, nested := q, q, q, q, q, q
	keep.Secret = "retained/key"
	other.Step = "other"
	foreign.Repo = "origin/team/other"
	counterpart.Step = "*"
	if q.Step == "*" {
		counterpart.Step = "build"
	}
	sibling.Secret += "-next"
	nested.Secret += "/nested"
	var protected []string
	for _, kind := range []identityname.Kind{identityname.StepCI, identityname.Step} {
		for _, target := range []apicontract.SecretPutArgs{q, keep, other, foreign, counterpart, sibling, nested} {
			account := secretAccountFor(t, target, kind)
			seedSecret(t, f, account, target.Secret)
			if target != q {
				protected = append(protected, storedPath(account, target.Secret))
			}
		}
	}
	return protected
}
