package service_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
)

func TestBearerActivationBindsCallerDigestAndNeverReturnsPlaintext(t *testing.T) {
	t.Parallel()
	p := people(t)
	_, key := publicKeys(t)
	secret := "caller-retained-one-time-bearer"
	digest := apicontract.SecretDigest(secret)
	q := apicontract.UplinkAddArgs{Scope: []string{"*"}, Identity: "bearer-agent", SshPublicKey: key, BearerDigest: digest}
	call, err := p.f.s.PrepareUplinkAdd(t.Context(), adminCaller(), q)
	out, err := execute(t.Context(), call, err)
	if err != nil {
		t.Fatal(err)
	}
	who, err := p.state.Authenticate(t.Context(), 0, digest)
	if err != nil || who.Identity != q.Identity {
		t.Fatal("caller digest was not activated", who, err)
	}
	raw, err := json.Marshal(out)
	if err != nil || strings.Contains(string(raw), secret) || strings.Contains(string(raw), `"token"`) {
		t.Fatal("service returned a bearer", string(raw), err)
	}
}

func TestBearerDigestRefusalLeavesNoIdentity(t *testing.T) {
	t.Parallel()
	for _, digest := range []string{"not-a-digest", strings.Repeat("a", 63), strings.Repeat("A", 64)} {
		t.Run(digest[:1], func(t *testing.T) {
			p := people(t)
			_, key := publicKeys(t)
			call, err := p.f.s.PrepareUplinkAdd(t.Context(), adminCaller(), apicontract.UplinkAddArgs{Scope: []string{"*"}, Identity: "invalid-bearer", SshPublicKey: key, BearerDigest: digest})
			if _, err = execute(t.Context(), call, err); err == nil {
				t.Fatal("invalid bearer digest accepted")
			}
			keys, err := p.state.IdentityKeys(t.Context(), "invalid-bearer")
			if err != nil || len(keys) != 0 {
				t.Fatal("refused digest left an identity", keys, err)
			}
		})
	}
}
