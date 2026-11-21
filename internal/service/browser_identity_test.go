package service_test

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/publicerr"
	"golang.org/x/crypto/ssh"
)

func TestBrowserOnlyUplinkAuthenticatesWithoutAnSSHKey(t *testing.T) {
	p := people(t)
	digest := apicontract.SecretDigest("browser-test-bearer")
	q := apicontract.UplinkAddArgs{Identity: "browser", Scope: []string{"origin"}, BearerDigest: digest}
	call, err := p.f.s.PrepareUplinkAdd(t.Context(), adminCaller(), q)
	identity, err := execute(t.Context(), call, err)
	if err != nil || !strings.HasPrefix(identity.Fingerprint, "bearer:") {
		t.Fatal("browser identity needs an SSH key", identity, err)
	}
	actor, err := p.state.Authenticate(t.Context(), 0, digest)
	if err != nil || actor.Identity != "browser" || actor.AuthMethod != "bearer" {
		t.Fatal("browser bearer cannot sign in", actor, err)
	}
	_, key := publicKeys(t)
	public, _, _, _, err := ssh.ParseAuthorizedKey([]byte(key))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.state.Key(t.Context(), ssh.FingerprintSHA256(public)); publicerr.Classify(err).Code() != publicerr.NotFound {
		t.Fatal("browser registration granted SSH access", err)
	}
	keys, err := p.state.IdentityKeys(t.Context(), "browser")
	if err != nil || len(keys) != 1 || keys[0].Fingerprint != identity.Fingerprint {
		t.Fatal("unexpected browser identity binding", keys, err)
	}
}

func TestUplinkWithoutKeyOrBearerNamesBothOptions(t *testing.T) {
	p := people(t)
	call, err := p.f.s.PrepareUplinkAdd(t.Context(), adminCaller(), apicontract.UplinkAddArgs{Identity: "browser"})
	problem := publicerr.Classify(err)
	if call != nil || problem.Code() != publicerr.InvalidInput || !strings.Contains(problem.Remedy(), "--ssh-public-key") || !strings.Contains(problem.Remedy(), "--bearer-file") {
		t.Fatal("missing credentials have no useful remedy", err)
	}
	keys, err := p.state.IdentityKeys(t.Context(), "browser")
	if err != nil || len(keys) != 0 {
		t.Fatal("refusal registered an identity", keys, err)
	}
}
