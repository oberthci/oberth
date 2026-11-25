package installer_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/secretstore"
	storefake "github.com/oberthci/oberth/internal/secretstore/fake"
	authv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ktesting "k8s.io/client-go/testing"
)

// An update of a bundled store whose backup role is not set up names the one
// step that enables full backups; once it is set up, the usual hint returns.
func TestUpdateNamesBackupSetupUntilTheRoleExists(t *testing.T) {
	t.Parallel()
	f := setup(t)
	if first := apply(t, f, prepare(t, f)); first.Updated {
		t.Fatal("fixture: the first install was an update")
	}
	f.client.PrependReactor("create", "serviceaccounts", func(action ktesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "token" {
			return false, nil, nil
		}
		return true, &authv1.TokenRequest{Status: authv1.TokenRequestStatus{Token: "backup-projection-fixture"}}, nil
	})
	role := http.StatusBadRequest
	f.engine.OpenStore = backupRoleStore(&role)
	missing := apply(t, f, prepare(t, f))
	if !missing.Updated || !strings.Contains(missing.StoreSnapshotHint, "secretstore setup --backup-only") {
		t.Fatal("update hid the backup setup step", missing.Updated, missing.StoreSnapshotHint)
	}
	reason, remedy, _ := strings.Cut(missing.StoreSnapshotHint, ". ")
	if len(reason)+1 > 40 || len(remedy) > 120 {
		t.Fatal("backup setup hint exceeds its bounds", missing.StoreSnapshotHint)
	}
	var lines strings.Builder
	if err := json.NewEncoder(&lines).Encode(missing); err != nil || !strings.Contains(lines.String(), "--backup-only") {
		t.Fatal("upgrade status omits the backup setup step", err)
	}
	role = http.StatusOK
	if ready := apply(t, f, prepare(t, f)); ready.StoreSnapshotHint != installer.StoreSnapshotHint {
		t.Fatal("update kept the setup step after the role existed", ready.StoreSnapshotHint)
	}
}

// backupRoleStore answers the backup role's login with *role and serves every
// other store call from a fake store.
func backupRoleStore(role *int) func(context.Context, secretstore.Config) (*secretstore.Client, func(), error) {
	backend := &storefake.Store{}
	return func(context.Context, secretstore.Config) (*secretstore.Client, func(), error) {
		client, err := secretstore.New(secretstore.Connection{URL: "https://store.oberth.svc", Transport: snapshotHTTP(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(raw))
			var args map[string]string
			if r.URL.Path != "/v1/auth/kubernetes/login" || json.Unmarshal(raw, &args) != nil || args["role"] != secretstore.BackupRole {
				return backend.RoundTrip(r)
			}
			body := `{"errors":["role not found"]}`
			if *role == http.StatusOK {
				body = `{"auth":{"client_token":"snapshot-session-fixture"}}`
			}
			return &http.Response{StatusCode: *role, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
		})})
		return client, func() {}, err
	}
}
