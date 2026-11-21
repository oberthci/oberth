package secretstore_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/secretstore"
)

type deleteTransport func(*http.Request) (*http.Response, error)

func (f deleteTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestWriterDeleteAcceptsAbsentWithoutExposingStoreResponse(t *testing.T) {
	for _, status := range []int{204, 404, 403, 503, 302} {
		t.Run(http.StatusText(status), func(t *testing.T) { checkDeleteStatus(t, status) })
	}
}
func checkDeleteStatus(t *testing.T, status int) {
	t.Helper()
	deletes := 0
	revokes := 0
	client, err := secretstore.New(secretstore.Connection{URL: "https://store.oberth.svc", Transport: deleteTransport(func(r *http.Request) (*http.Response, error) {
		assertDeleteRequest(t, r)
		code, body := 200, `{"auth":{"client_token":"session"}}`
		switch r.URL.Path {
		case "/v1/oberth/steps/step-one/key":
			deletes++
			code = status
			body = "private-store-response"
		case "/v1/auth/token/revoke-self":
			revokes++
		}
		return &http.Response{StatusCode: code, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	writer := secretstore.Writer{Client: client, Config: config(), Token: func(context.Context) ([]byte, error) { return []byte("projected"), nil }}
	err = writer.Delete(t.Context(), "step-one", "key")
	if (err == nil) != (status == 204 || status == 404) {
		t.Fatal("delete status differs", status, err)
	}
	if err != nil && strings.Contains(err.Error(), "private-store-response") {
		t.Fatal("store response escaped")
	}
	if deletes != 1 || revokes != 1 {
		t.Fatal("delete/session cleanup count differs", deletes, revokes)
	}
}

func TestWriterDeleteDoesNotAcceptMissingLogin(t *testing.T) {
	deleted := false
	client, err := secretstore.New(secretstore.Connection{URL: "https://store.oberth.svc", Transport: deleteTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method == "DELETE" {
			deleted = true
		}
		return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader("private-store-response"))}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	writer := secretstore.Writer{Client: client, Config: config(), Token: func(context.Context) ([]byte, error) { return []byte("projected"), nil }}
	if err := writer.Delete(t.Context(), "step-one", "key"); err == nil || deleted {
		t.Fatal("missing login authorized delete", err)
	}
}

func assertDeleteRequest(t *testing.T, r *http.Request) {
	t.Helper()
	if r.Method == "GET" {
		t.Fatal("delete read a value")
	}
	if strings.Contains(r.URL.Path, "/steps/") && (r.Method != "DELETE" || r.Header.Get("X-Vault-Token") == "") {
		t.Fatal("delete method or authentication changed")
	}
}
