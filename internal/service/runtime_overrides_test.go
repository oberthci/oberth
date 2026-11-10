package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

const runtimeTestRepo = "codeberg/acme/oberth"

func runtimeWIFFixture(t *testing.T) (*controlFixture, *API, *argojob.ReleaseWIFRuntime) {
	t.Helper()
	f := newControlFixture(t)
	runtime := argojob.NewReleaseWIFRuntime(argojob.Config{
		Namespace: "oberth-argo", PipelineServiceAccount: "pipeline", CredentialedServiceAccount: "credentialed", CISecretsServiceAccount: "ci-secrets", ExecutorServiceAccount: "executor",
		ReleaseWIF: &argojob.ReleaseWIFConfig{
			Namespace: "oberth-argo",
			Roles: map[string]argojob.ReleaseWIFRole{
				"image-writer": {Provider: "projects/1/locations/global/workloadIdentityPools/images/providers/node", ServiceAccount: "image-writer@example-project.iam.gserviceaccount.com"},
			},
			Repositories: map[string]argojob.ReleaseWIFRepository{
				"github/retired/old": {ServiceAccountName: "old-release", Templates: map[string]string{"publish": "image-writer"}},
			},
		},
	})
	control := f.api(t)
	control.releaseWIFOverrides, control.releaseWIF = f.store, runtime
	return f, control, runtime
}

func TestRuntimeAdminToolsRequireUnscopedAdminAndMutationGate(t *testing.T) {
	_, control, _ := runtimeWIFFixture(t)
	ctx := t.Context()
	calls := map[string]string{
		"release_wif_list":   `{}`,
		"release_wif_grant":  `{"repo":"oberth","service_account_name":"oberth-release","templates":{"publish":"image-writer"},"reason":"r"}`,
		"release_wif_revoke": `{"repo":"github/retired/old","reason":"r"}`,
		"release_wif_reset":  `{"repo":"github/retired/old","reason":"r"}`,
	}
	for name, raw := range calls {
		for _, actor := range []api.Actor{{Identity: "dev"}, {Identity: "scoped", Admin: true, Scope: []string{"codeberg"}}} {
			if _, err := control.CallTool(ctx, actor, name, json.RawMessage(raw)); !errors.Is(err, ErrForbidden) {
				t.Fatalf("%s by %+v: %v", name, actor, err)
			}
		}
		if MutatingTool(name) != (name != "release_wif_list") {
			t.Fatalf("%s mutation classification", name)
		}
	}
	denied := errors.New("audit gate refused")
	control.mutationGate = func(context.Context) error { return denied }
	admin := api.Actor{Identity: "admin", Admin: true}
	for _, name := range []string{"release_wif_grant", "release_wif_revoke"} {
		if _, err := control.CallTool(ctx, admin, name, json.RawMessage(calls[name])); !errors.Is(err, denied) {
			t.Fatalf("%s bypassed the audit gate: %v", name, err)
		}
	}
}

func TestReleaseWIFRuntimeGrantRevokeResetWithoutRestart(t *testing.T) {
	f, control, runtime := runtimeWIFFixture(t)
	ctx := t.Context()
	admin := api.Actor{Identity: "admin", Admin: true}
	call := func(name, raw string) (map[string]any, error) {
		value, err := control.CallTool(ctx, admin, name, json.RawMessage(raw))
		if err != nil {
			return nil, err
		}
		body, _ := json.Marshal(value)
		var decoded map[string]any
		_ = json.Unmarshal(body, &decoded)
		return decoded, nil
	}
	for _, raw := range []string{
		`{"repo":"oberth","service_account_name":"oberth-release","templates":{"publish":"cluster-admin"},"reason":"r"}`,
		`{"repo":"oberth","service_account_name":"credentialed","templates":{"publish":"image-writer"},"reason":"r"}`,
		`{"repo":"oberth","service_account_name":"old-release","templates":{"publish":"image-writer"},"reason":"r"}`,
		`{"repo":"oberth","service_account_name":"oberth-release","templates":{"publish":"image-writer"},"step_service_accounts":{"publish":"x"},"reason":"r"}`,
		`{"repo":"oberth","service_account_name":"oberth-release","templates":{"publish":"image-writer"}}`,
	} {
		if _, err := call("release_wif_grant", raw); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	if overrides, _ := f.store.ReleaseWIFOverrides(ctx); len(overrides) != 0 {
		t.Fatalf("refused grants were stored: %+v", overrides)
	}
	view, err := call("release_wif_grant", `{"repo":"oberth","service_account_name":"oberth-release","templates":{"publish":"image-writer"},"reason":"absorb e2e"}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := view["effective"].(map[string]any)[runtimeTestRepo]; !ok {
		t.Fatalf("grant not effective in response: %+v", view)
	}
	if runtime.Snapshot().Repositories[runtimeTestRepo].Templates["publish"] != "image-writer" {
		t.Fatal("grant not applied to the admission snapshot")
	}
	// A retired, unregistered boot entry is revocable without Helm.
	if _, err := call("release_wif_revoke", `{"repo":"github/retired/old","reason":"repository deleted upstream"}`); err != nil {
		t.Fatal(err)
	}
	if _, ok := runtime.Snapshot().Repositories["github/retired/old"]; ok {
		t.Fatal("revoked boot entry still effective")
	}
	if _, err := call("release_wif_revoke", `{"repo":"github/retired/old","reason":"again"}`); !errors.Is(err, store.ErrInvalidState) {
		t.Fatalf("second revoke: %v", err)
	}
	if _, err := call("release_wif_revoke", `{"repo":"github/never/granted","reason":"r"}`); !errors.Is(err, store.ErrInvalidState) {
		t.Fatalf("revoke of absent entry: %v", err)
	}
	if _, err := call("release_wif_reset", `{"repo":"github/retired/old","reason":"restore helm default"}`); err != nil {
		t.Fatal(err)
	}
	if _, ok := runtime.Snapshot().Repositories["github/retired/old"]; !ok {
		t.Fatal("reset did not restore the boot entry")
	}
	if _, err := call("release_wif_reset", `{"repo":"github/retired/old","reason":"r"}`); !errors.Is(err, store.ErrInvalidState) {
		t.Fatalf("reset without override: %v", err)
	}
	// Overrides survive a restart: a fresh runtime reloads them from the store.
	fresh := argojob.NewReleaseWIFRuntime(argojob.Config{Namespace: "oberth-argo", ReleaseWIF: runtime.Base()})
	if inactive, err := ReloadReleaseWIF(ctx, f.store, fresh); err != nil || len(inactive) != 0 || fresh.Snapshot().Repositories[runtimeTestRepo].ServiceAccountName != "oberth-release" {
		t.Fatalf("reload: %+v %v", inactive, err)
	}
	var actions []string
	audit, _, err := f.store.ListAuditActions(ctx, model.AuditListFilter{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range audit {
		if strings.HasPrefix(entry.Action, "release_wif.") {
			actions = append(actions, entry.Action+"@"+entry.Actor)
		}
	}
	if strings.Join(actions, ",") != "release_wif.reset@admin,release_wif.revoke@admin,release_wif.grant@admin" {
		t.Fatalf("audit = %v", actions)
	}
	if _, err := f.store.SetRepositoryArchived(ctx, "admin", f.repo.Name, true); err != nil {
		t.Fatal(err)
	}
	if _, err := call("release_wif_grant", `{"repo":"oberth","service_account_name":"oberth-release","templates":{"publish":"image-writer"},"reason":"r"}`); !errors.Is(err, store.ErrRepositoryArchived) {
		t.Fatalf("archived grant: %v", err)
	}
}

// cancelAfterCommit ends the request right after the store commits, as an
// MCP client that disconnects or times out would.
type cancelAfterCommit struct {
	*store.Store
	cancel context.CancelFunc
}

func (s cancelAfterCommit) SetReleaseWIFOverride(ctx context.Context, actor string, spec model.ReleaseWIFOverrideSpec) (model.ReleaseWIFOverride, error) {
	defer s.cancel()
	return s.Store.SetReleaseWIFOverride(ctx, actor, spec)
}

func TestRuntimeOverridesTakeEffectWhenRequestEndsAfterCommit(t *testing.T) {
	f, control, runtime := runtimeWIFFixture(t)
	admin := api.Actor{Identity: "admin", Admin: true}
	ctx, cancel := context.WithCancel(t.Context())
	control.releaseWIFOverrides = cancelAfterCommit{Store: f.store, cancel: cancel}
	if _, err := control.CallTool(ctx, admin, "release_wif_grant", json.RawMessage(`{"repo":"oberth","service_account_name":"oberth-release","templates":{"publish":"image-writer"},"reason":"client times out"}`)); err != nil {
		t.Fatalf("committed grant reported failure: %v", err)
	}
	if runtime.Snapshot().Repositories[runtimeTestRepo].ServiceAccountName != "oberth-release" {
		t.Fatal("committed grant is not applied to admission")
	}
}

// release_wif_list publishes the stored set, so any drift between the store
// and the admission map is repaired by listing.
func TestReleaseWIFListResyncsAdmissionMap(t *testing.T) {
	f, control, runtime := runtimeWIFFixture(t)
	ctx := t.Context()
	if _, err := f.store.SetReleaseWIFOverride(ctx, "admin", model.ReleaseWIFOverrideSpec{Repo: "github/retired/old", Revoked: true, Reason: "stored by another path"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := runtime.Snapshot().Repositories["github/retired/old"]; !ok {
		t.Fatal("fixture: store write must not publish by itself")
	}
	value, err := control.CallTool(ctx, api.Actor{Identity: "admin", Admin: true}, "release_wif_list", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := runtime.Snapshot().Repositories["github/retired/old"]; ok {
		t.Fatal("release_wif_list did not re-sync the stored revoke")
	}
	if effective := value.(map[string]any)["effective"].(map[string]json.RawMessage); effective["github/retired/old"] != nil {
		t.Fatalf("view effective = %v", effective)
	}
}

// Revoking or shadowing a Helm entry audits the Helm grant it replaced.
func TestReleaseWIFAuditRecordsReplacedEffectiveEntry(t *testing.T) {
	f, control, _ := runtimeWIFFixture(t)
	ctx := t.Context()
	admin := api.Actor{Identity: "admin", Admin: true}
	for _, call := range []struct{ name, raw string }{
		{"release_wif_revoke", `{"repo":"github/retired/old","reason":"retired"}`},
		{"release_wif_reset", `{"repo":"github/retired/old","reason":"restore"}`},
	} {
		if _, err := control.CallTool(ctx, admin, call.name, json.RawMessage(call.raw)); err != nil {
			t.Fatalf("%s: %v", call.name, err)
		}
	}
	audit, _, err := f.store.ListAuditActions(ctx, model.AuditListFilter{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	helm := `"service_account_name":"old-release"`
	seen := 0
	for _, entry := range audit {
		var details struct {
			Previous          json.RawMessage `json:"previous"`
			PreviousEffective json.RawMessage `json:"previous_effective"`
			Effective         json.RawMessage `json:"effective"`
		}
		if err := json.Unmarshal([]byte(entry.Details), &details); err != nil {
			continue
		}
		switch entry.Action {
		case "release_wif.revoke":
			seen++
			if details.Previous != nil || !strings.Contains(string(details.PreviousEffective), helm) || string(details.Effective) != "null" {
				t.Fatalf("revoke audit lacks the Helm grant: %s", entry.Details)
			}
		case "release_wif.reset":
			seen++
			if string(details.PreviousEffective) != "null" || !strings.Contains(string(details.Effective), helm) {
				t.Fatalf("reset audit lacks the restored Helm grant: %s", entry.Details)
			}
		}
	}
	if seen != 2 {
		t.Fatalf("audit entries seen = %d", seen)
	}
}
