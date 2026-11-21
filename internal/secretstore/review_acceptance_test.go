package secretstore_test

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/secretstore"
	"github.com/oberthci/oberth/internal/secretstore/fake"
)

func TestReviewPolicyAndPathIsolation(t *testing.T) {
	for _, shape := range []string{"missing policy", "empty policy", "global read", "missing role", "wildcard namespace", "default policy enabled"} {
		t.Run(shape, func(t *testing.T) { reviewPolicyShape(t, shape) })
	}
	for _, path := range []string{".", "x/./value", "../other/value", "x/../../other/value", "%2e%2e/other", "x//value", "/other/value", "x/*", "x/+", "x/?", "x/[ab]", "x\\..\\other", "x/..", "x/．", "x/а", "x/\x00"} {
		t.Run("path/"+path, func(t *testing.T) { reviewPathShape(t, path) })
	}
	reviewPolicyDelimiter(t)
}
func reviewPolicyShape(t *testing.T, shape string) {
	t.Helper()
	backend := &fake.Store{}
	client, err := secretstore.New(secretstore.Connection{URL: "https://store.oberth.svc", Transport: backend})
	if err != nil {
		t.Fatal(err)
	}
	if err = client.Setup(t.Context(), []byte("synthetic-review-operator"), config()); err != nil {
		t.Fatal(err)
	}
	reviewAlterAuthority(backend, shape)
	before := len(backend.Calls)
	if err = client.Check(t.Context(), []byte("synthetic-review-operator"), config()); err == nil {
		t.Fatal("disabled authority passed check")
	}
	for _, call := range backend.Calls[before:] {
		if call.Method != "GET" {
			t.Fatal("authority check performed a write")
		}
	}
}
func reviewPathShape(t *testing.T, path string) {
	t.Helper()
	writer, backend := boundedWriter(t)
	if err := writer.Put(t.Context(), "step-one", path, []byte("synthetic-review-value")); err == nil || len(backend.Calls) != 0 {
		t.Fatal("unsafe path reached writer transport")
	}
	claim := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"system:serviceaccount:oberth-runs:step-one"}`))
	values, err := writer.Client.Fetch(t.Context(), config(), []byte("header."+claim+".signature"), []string{path})
	if err == nil || values != nil {
		t.Fatal("unsafe fetch path published a value")
	}
	for _, call := range backend.Calls {
		if call.Method == "GET" {
			t.Fatal("unsafe fetch path reached store data")
		}
	}
}
func reviewPolicyDelimiter(t *testing.T) {
	t.Helper()
	// The delimiter is part of the external ACL, so a prefix account has no sibling match.
	backend := &fake.Store{}
	client, err := secretstore.New(secretstore.Connection{URL: "https://store.oberth.svc", Transport: backend})
	if err != nil {
		t.Fatal(err)
	}
	if err = client.Setup(t.Context(), []byte("synthetic-review-operator"), config()); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(backend.Resources["/sys/policies/acl/oberth-step"])
	if !strings.Contains(string(raw), "metadata.service_account_name}}/*") {
		t.Fatal("account prefix lost its slash delimiter")
	}
}

func reviewAlterAuthority(backend *fake.Store, shape string) {
	policy := "/sys/policies/acl/oberth-step"
	role := "/auth/kubernetes/role/oberth-step"
	switch shape {
	case "missing policy":
		delete(backend.Resources, policy)
	case "empty policy":
		backend.Resources[policy]["policy"] = ""
	case "global read":
		backend.Resources[policy]["policy"] = `path "oberth/steps/*" { capabilities = ["read"] }`
	case "missing role":
		delete(backend.Resources, role)
	case "wildcard namespace":
		backend.Resources[role]["bound_service_account_namespaces"] = []string{"*"}
	case "default policy enabled":
		backend.Resources[role]["token_no_default_policy"] = false
	}
}
