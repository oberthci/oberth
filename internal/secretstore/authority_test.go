package secretstore_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/secretstore"
	"github.com/oberthci/oberth/internal/secretstore/fake"
)

// Covers: REQ-C9-022
func TestExactSecretStoreAuthorityContract(t *testing.T) {
	backend := &fake.Store{}
	client, err := secretstore.New(secretstore.Connection{URL: "https://store.oberth.svc", Transport: backend})
	if err != nil {
		t.Fatal(err)
	}
	if err = client.Setup(t.Context(), []byte("operator"), config()); err != nil {
		t.Fatal(err)
	}
	writer := backend.Resources["/sys/policies/acl/oberth-writer"]["policy"].(string)
	if writer != `path "oberth/steps/*" { capabilities = ["create", "update", "delete"] }
path "auth/token/revoke-self" { capabilities = ["update"] }` {
		t.Fatal("writer policy grants unexpected authority")
	}
	step := backend.Resources["/sys/policies/acl/oberth-step"]["policy"].(string)
	if step != `path "oberth/steps/{{identity.entity.aliases.auth_kubernetes_fixture.metadata.service_account_name}}/*" { capabilities = ["read"] }
path "auth/token/revoke-self" { capabilities = ["update"] }` {
		t.Fatal("step policy is not account-templated")
	}
	for _, tc := range []struct{ role, namespace, account, audience string }{{"oberth-writer", "oberth", "oberth", "oberth-store"}, {"oberth-step", "oberth-runs", "*", "oberth-store"}} {
		r := backend.Resources["/auth/kubernetes/role/"+tc.role]
		for key, want := range map[string]any{"bound_service_account_names": []string{tc.account}, "bound_service_account_namespaces": []string{tc.namespace}, "bound_service_account_namespace_selector": "", "token_policies": []string{tc.role}, "audience": tc.audience, "token_no_default_policy": true, "token_ttl": 600, "token_max_ttl": 600, "alias_name_source": "serviceaccount_uid"} {
			a, _ := json.Marshal(r[key])
			b, _ := json.Marshal(want)
			if string(a) != string(b) {
				t.Fatal("role differs", tc.role, key)
			}
		}
	}
	checkSetupDrift(t, client, backend)
}
func checkSetupDrift(t *testing.T, client *secretstore.Client, backend *fake.Store) {
	t.Helper()
	var err error
	before := len(backend.Calls)
	if err = client.Setup(t.Context(), []byte("operator"), config()); err != nil {
		t.Fatal(err)
	}
	for _, call := range backend.Calls[before:] {
		if call.Method != "GET" {
			t.Fatal("idempotent setup wrote", call.Path)
		}
	}
	backend.Resources["/auth/kubernetes/role/oberth-writer"]["bound_service_account_names"] = []string{"*"}
	before = len(backend.Calls)
	if err = client.Setup(t.Context(), []byte("operator"), config()); err == nil || !strings.Contains(err.Error(), "/auth/kubernetes/role/oberth-writer") {
		t.Fatal("drift not named", err)
	}
	for _, call := range backend.Calls[before:] {
		if call.Method != "GET" {
			t.Fatal("drift overwritten")
		}
	}
}

func TestWriterStoresAndPermanentlyDeletesWithoutRead(t *testing.T) {
	backend := &fake.Store{}
	client, err := secretstore.New(secretstore.Connection{URL: "https://store.oberth.svc", Transport: backend})
	if err != nil {
		t.Fatal(err)
	}
	writer := secretstore.Writer{Client: client, Config: config(), Token: func(context.Context) ([]byte, error) { return []byte("projected-token"), nil }}
	if err = writer.Put(t.Context(), "step-one", "credential", []byte("test-value")); err != nil {
		t.Fatal(err)
	}
	if backend.Resources["/oberth/steps/step-one/credential"]["value"] != "dGVzdC12YWx1ZQ==" {
		t.Fatal("KV v1 value not stored")
	}
	if err = writer.Delete(t.Context(), "step-one", "credential"); err != nil {
		t.Fatal(err)
	}
	if _, ok := backend.Resources["/oberth/steps/step-one/credential"]; ok {
		t.Fatal("revoked value retained")
	}
	for _, call := range backend.Calls {
		if call.Method == "GET" {
			t.Fatal("writer read a value")
		}
	}
}
