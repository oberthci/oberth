package service_test

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/publicerr"
)

func TestUplinkCreationRequiresExplicitScopeBeforeIssuance(t *testing.T) {
	for index, scope := range [][]string{nil, {}, {"*", "origin"}} {
		for _, kind := range []string{"ssh", "certificate", "browser"} {
			t.Run(kind+"/"+[]string{"omitted", "empty", "mixed"}[index], func(t *testing.T) {
				checkUplinkScopeRefusal(t, scope, kind)
			})
		}
	}
}

func checkUplinkScopeRefusal(t *testing.T, scope []string, kind string) {
	t.Helper()
	p := people(t)
	csr, key := publicKeys(t)
	q := apicontract.UplinkAddArgs{Identity: "unscoped", Scope: scope}
	switch kind {
	case "ssh":
		q.SshPublicKey = key
	case "certificate":
		q.SshPublicKey, q.CSR = key, csr
	case "browser":
		q.BearerDigest = apicontract.SecretDigest("test-only-unscoped")
	}
	call, err := p.f.s.PrepareUplinkAdd(t.Context(), adminCaller(), q)
	_, err = execute(t.Context(), call, err)
	if err == nil || publicerr.Classify(err).Code() != publicerr.InvalidInput {
		t.Fatal("implicit all-repository identity accepted", err)
	}
	if len(scope) == 0 && !strings.Contains(publicerr.Classify(err).Remedy(), "--scope") {
		t.Fatal("scope refusal has no remedy", err)
	}
	rows, err := p.state.IdentityKeys(t.Context(), q.Identity)
	if err != nil || len(rows) != 0 || len(p.issuer.Identities) != 0 {
		t.Fatal("refused enrollment changed authority", err)
	}
}

func TestUplinkExplicitScopeIsFrozenAtPreparation(t *testing.T) {
	p := people(t)
	_, key := publicKeys(t)
	scope := []string{"origin"}
	call, err := p.f.s.PrepareUplinkAdd(t.Context(), adminCaller(), apicontract.UplinkAddArgs{Identity: "scoped", Scope: scope, SshPublicKey: key})
	scope[0] = "*"
	got, err := execute(t.Context(), call, err)
	if err != nil || len(got.Scope) != 1 || got.Scope[0] != "origin" {
		t.Fatal("prepared scope changed", got.Scope, err)
	}
}
