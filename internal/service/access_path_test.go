package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"reflect"
	"testing"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/oberthci/oberth/internal/api"
)

func TestAccessRevokeUsesExactListedPathAndTuple(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := newControlFixture(t)
	client := fake.NewClientset()
	service := fixture.api(t)
	service.secretAccess = fixture.store
	service.secretAccessReconciler = NewAccessReconciler(client, "oberth", fixture.store, log.New(io.Discard, "", 0))
	actor := api.Actor{Identity: "admin@fixture", Admin: true}
	const repo = "codeberg/acme/oberth"
	const path = "oberth/data/release/cosign-secret"
	type tuple struct {
		Repo   string `json:"repo"`
		Step   string `json:"step"`
		Secret string `json:"secret"`
	}
	call := func(name string, value tuple) (any, error) {
		t.Helper()
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return service.CallTool(ctx, actor, name, raw)
	}
	// A wildcard step, a named step, another repository and another full path
	// must remain distinct even when they share a path suffix.
	for _, entry := range []tuple{
		{repo, "*", path},
		{repo, "release-sign", path},
		{"codeberg/acme/neighbor", "*", path},
		{repo, "*", "other/data/release/cosign-secret"},
	} {
		if _, err := call("access_allow", entry); err != nil {
			t.Fatal(err)
		}
	}
	before, err := fixture.store.SecretAccessList(ctx, "", false)
	if err != nil || len(before) != 4 {
		t.Fatalf("initial grants = %v, error = %v", before, err)
	}
	client.ClearActions()
	for _, wrong := range []string{
		"release/cosign-secret", "oberth/release/cosign-secret",
		"/" + path, path + "/", "oberth/data/release/./cosign-secret",
	} {
		if _, err := call("access_revoke", tuple{repo, "*", wrong}); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("revoke %q: error = %v, want invalid input", wrong, err)
		}
	}
	if actions := client.Actions(); len(actions) != 0 {
		t.Fatalf("inexact paths reached ConfigMap: %v", actions)
	}
	after, err := fixture.store.SecretAccessList(ctx, "", false)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("refused paths changed grants: %v, error = %v", after, err)
	}
	listed, err := service.CallTool(ctx, actor, "access_list", json.RawMessage(`{"repo":"codeberg/acme/oberth"}`))
	if err != nil {
		t.Fatal(err)
	}
	var target api.AccessGrantResponse
	for _, grant := range listed.(api.AccessListResponse).Grants {
		if grant.Step == "*" && grant.Secret == path {
			target = grant
		}
	}
	if target.ID == 0 {
		t.Fatal("full-path wildcard grant missing from access_list")
	}
	result, err := call("access_revoke", tuple{target.Repo, target.Step, target.Secret})
	if err != nil {
		t.Fatal(err)
	}
	revoked := result.(api.AccessGrantResponse)
	if revoked.ID != target.ID || revoked.RevokedAt == nil || revoked.Warning == "" {
		t.Fatalf("exact listed tuple revoked wrong grant or lost policy-sync advisory: %+v", revoked)
	}
	after, err = fixture.store.SecretAccessList(ctx, "", false)
	if err != nil || len(after) != 3 {
		t.Fatalf("remaining grants = %v, error = %v", after, err)
	}
	for _, grant := range after {
		found := false
		for _, original := range before {
			if grant.ID == original.ID && original.ID != target.ID && reflect.DeepEqual(grant, original) {
				found = true
			}
		}
		if !found {
			t.Fatalf("unrelated grant changed: %+v", grant)
		}
	}
}
