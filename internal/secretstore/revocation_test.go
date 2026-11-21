package secretstore

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestRuntimeRevocationPreservesOperationResult(t *testing.T) {
	for _, operation := range []string{"write", "delete", "fetch"} {
		t.Run(operation, func(t *testing.T) { runtimeRevokeCase(t, operation) })
	}
}
func runtimeRevokeCase(t *testing.T, operation string) {
	t.Helper()
	revoked := false
	client, err := New(Connection{URL: "https://store.oberth.svc", Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		status, body := 200, `{"data":{"value":"dGVzdA=="}}`
		switch r.URL.Path {
		case "/v1/auth/kubernetes/login":
			body = `{"auth":{"client_token":"session"}}`
		case "/v1/auth/token/revoke-self":
			revoked = true
			status = 403
			body = `{}`
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Mount: "oberth", AuthMount: "kubernetes", WriterRole: "oberth-writer", StepRole: "oberth-step", ServerNamespace: "oberth", PipelineNamespace: "oberth-runs", ServerAccount: "oberth", Audience: "oberth-store"}
	if operation == "write" || operation == "delete" {
		writer := Writer{Client: client, Config: cfg, Token: func(context.Context) ([]byte, error) { return []byte("projected"), nil }}
		if operation == "delete" {
			err = writer.Delete(t.Context(), "step-one", "key")
		} else {
			err = writer.Put(t.Context(), "step-one", "key", []byte("test"))
		}
	} else {
		claim := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"system:serviceaccount:oberth-runs:step-one"}`))
		var values map[string][]byte
		values, err = client.Fetch(t.Context(), cfg, []byte("header."+claim+".signature"), []string{"key"})
		if values != nil {
			t.Fatal("failed cleanup published values")
		}
	}
	if !revoked {
		t.Fatal("session revocation was not attempted")
	}
	wantFailure := operation == "fetch"
	if (err != nil) != wantFailure {
		t.Fatal("revocation changed the operation result", operation, err)
	}
}
