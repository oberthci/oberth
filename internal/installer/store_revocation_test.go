package installer_test

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/secretstore"
	storefake "github.com/oberthci/oberth/internal/secretstore/fake"
)

type rootRevocation struct {
	backend           *storefake.Store
	lostReply         bool
	attempts, lookups int
}

func (s *rootRevocation) RoundTrip(r *http.Request) (*http.Response, error) {
	code := 0
	switch r.URL.Path {
	case "/v1/auth/token/revoke-self":
		s.attempts++
		if s.lostReply && s.attempts == 1 {
			s.backend.Revoked = true
			code = 503
		} else {
			code = 403
		}
	case "/v1/auth/token/lookup-self":
		s.lookups++
		if s.backend.Revoked {
			code = 403
		} else {
			code = 200
		}
	}
	if code != 0 {
		return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	}
	return s.backend.RoundTrip(r)
}
func TestBootstrapRootRevocationProof(t *testing.T) {
	t.Parallel()
	for _, lostReply := range []bool{false, true} {
		t.Run(boolText(lostReply), func(t *testing.T) { bootstrapRevokeCase(t, lostReply) })
	}
}
func bootstrapRevokeCase(t *testing.T, lostReply bool) {
	t.Helper()
	f, backend := storeEngine(t)
	transport := &rootRevocation{backend: backend, lostReply: lostReply}
	f.engine.OpenStore = func(context.Context, secretstore.Config) (*secretstore.Client, func(), error) {
		client, err := secretstore.New(secretstore.Connection{URL: "https://store.oberth.svc", Transport: transport})
		return client, func() {}, err
	}
	p := prepare(t, f)
	if _, err := f.engine.Apply(t.Context(), p, p.Digest(), nil); err == nil {
		t.Fatal("unproven revocation reported success")
	}
	pending := filepath.Join(f.dir, "store-init.json")
	if _, err := os.Stat(pending); err != nil {
		t.Fatal("resume authority discarded", err)
	}
	if !lostReply {
		if transport.lookups != 1 {
			t.Fatal("root validity was not checked")
		}
		return
	}
	apply(t, f, prepare(t, f))
	if transport.lookups != 1 {
		t.Fatal("lost reply accepted without invalid-root proof")
	}
	if _, err := os.Stat(pending); !os.IsNotExist(err) {
		t.Fatal("revoked root still retained", err)
	}
}

func TestInterruptedBootstrapRequiresManualRebootUnseal(t *testing.T) {
	t.Parallel()
	f, backend := storeEngine(t)
	backend.Fail = "/auth/token/revoke-self"
	p := prepare(t, f)
	if _, err := f.engine.Apply(t.Context(), p, p.Digest(), nil); err == nil {
		t.Fatal("bootstrap fixture did not stop")
	}
	backend.Fail = ""
	backend.Sealed = true
	p = prepare(t, f)
	_, err := f.engine.Apply(t.Context(), p, p.Digest(), nil)
	if err == nil || !backend.Sealed {
		t.Fatal("interrupted bootstrap reopened the store after reboot", err)
	}
	if _, err = f.engine.Unseal(t.Context()); err != nil {
		t.Fatal(err)
	}
	if backend.Sealed {
		t.Fatal("explicit unseal did not open pending bootstrap")
	}
	apply(t, f, prepare(t, f))
	if !backend.Revoked {
		t.Fatal("resumed bootstrap did not retire root")
	}
}
