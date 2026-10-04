package secretstore

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestIdentityCASAndSessionRevocation(t *testing.T) {
	mock := newMockVault(t)
	mux := mock.Config.Handler.(*http.ServeMux)
	version := 0
	var stored map[string]any
	mux.HandleFunc("/v1/oberth/data/identities/oberth/server", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Vault-Token") != testLoginToken {
			t.Error("missing authenticated identity session")
			w.WriteHeader(403)
			return
		}
		if r.Method == "GET" {
			if version == 0 {
				w.WriteHeader(404)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"data": stored, "metadata": map[string]any{"version": version}}})
			return
		}
		var body struct {
			Data    map[string]any `json:"data"`
			Options struct {
				CAS int `json:"cas"`
			} `json:"options"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Options.CAS != version {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"errors":["fixture-private-value"]}`))
			return
		}
		version++
		stored = body.Data
		_, _ = w.Write([]byte(`{"data":{}}`))
	})
	client, err := New(mock.config())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	values, v, err := client.ReadIdentity(ctx, "oberth", "server")
	if err != nil || values != nil || v != 0 {
		t.Fatalf("fresh identity: %v %d", err, v)
	}
	if err = client.WriteIdentity(ctx, "oberth", "server", 0, map[string][]byte{"tls.key": []byte("fixture-private-value")}); err != nil {
		t.Fatal(err)
	}
	values, v, err = client.ReadIdentity(ctx, "oberth", "server")
	if err != nil || v != 1 || string(values["tls.key"]) != "fixture-private-value" {
		t.Fatal("identity did not roundtrip")
	}
	err = client.WriteIdentity(ctx, "oberth", "server", 0, map[string][]byte{"tls.key": []byte("replacement")})
	if err == nil || strings.Contains(err.Error(), "fixture-private-value") {
		t.Fatalf("CAS error missing or exposed body: %v", err)
	}
	if mock.logins.Load() != mock.revokes.Load() {
		t.Fatal("identity session not revoked")
	}
}

func TestIdentityPathRefusesTraversal(t *testing.T) {
	for _, part := range []string{"", "..", "other/key", "a*b", "a+b", "a b", "a\nb"} {
		if _, err := IdentityPath("oberth", part); err == nil {
			t.Errorf("accepted %q", part)
		}
	}
}
