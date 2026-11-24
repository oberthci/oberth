package secretstore_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/secretstore"
	"github.com/oberthci/oberth/internal/secretstore/fake"
)

type snapshotTransport func(*http.Request) (*http.Response, error)

func (f snapshotTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSnapshotRoleHasOnlyReadAndRevocation(t *testing.T) {
	backend := &fake.Store{}
	client, err := secretstore.New(secretstore.Connection{URL: "https://store.test", Transport: backend})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err = client.Setup(t.Context(), []byte("operator-fixture"), config()); err != nil {
		t.Fatal(err)
	}
	role := backend.Resources["/auth/kubernetes/role/oberth-backup"]
	policy := backend.Resources["/sys/policies/acl/oberth-backup"]
	want := `path "sys/storage/raft/snapshot" { capabilities = ["read"] }
path "auth/token/revoke-self" { capabilities = ["update"] }`
	if policy["policy"] != want {
		t.Fatal("backup policy is absent or grants extra authority")
	}
	for field, value := range map[string]any{"bound_service_account_names": []string{"oberth-backup"}, "bound_service_account_namespaces": []string{"oberth"}, "token_policies": []string{"oberth-backup"}, "token_no_default_policy": true, "token_max_ttl": 600} {
		got, _ := json.Marshal(role[field])
		expected, _ := json.Marshal(value)
		if string(got) != string(expected) {
			t.Fatal("backup login can widen authority", field)
		}
	}
}

func TestSnapshotBoundsErrorsAndRevokesEveryLogin(t *testing.T) {
	for _, scenario := range []string{"success", "oversize", "refused"} {
		t.Run(scenario, func(t *testing.T) {
			snapshotBoundsCase(t, scenario)
		})
	}
}

func snapshotBoundsCase(t *testing.T, scenario string) {
	t.Helper()
	revoked := false
	transport := snapshotTransport(func(r *http.Request) (*http.Response, error) {
		body, status := "", http.StatusOK
		switch r.URL.Path {
		case "/v1/auth/kubernetes/login":
			body = `{"auth":{"client_token":"private-session"}}`
		case "/v1/sys/storage/raft/snapshot":
			body = "private-snapshot"
			if scenario == "refused" {
				status = http.StatusForbidden
			}
		case "/v1/auth/token/revoke-self":
			revoked = true
		default:
			t.Fatal("unexpected request")
		}
		return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	client, err := secretstore.New(secretstore.Connection{URL: "https://store.test", Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	limit := int64(100)
	if scenario == "oversize" {
		limit = 2
	}
	var out bytes.Buffer
	err = client.Snapshot(t.Context(), secretstore.SnapshotRequest{Config: config(), JWT: []byte("projection-fixture"), Limit: limit}, &out)
	if (err == nil) != (scenario == "success") || !revoked {
		t.Fatal("snapshot bounds or cleanup failed", err)
	}
	if err != nil && strings.Contains(err.Error(), "private-") {
		t.Fatal("snapshot refusal exposed response data")
	}
}
