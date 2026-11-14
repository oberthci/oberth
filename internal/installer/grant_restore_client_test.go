package installer

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

func TestSecretsOnlyRestoreUsesRealClient(t *testing.T) {
	archive, _, _, target := reinstallFixture(t)
	db, _, cleanup, err := target.openGrantDatabase(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cleanup() }()
	// Both actions must cross the client boundary: remove this excess grant
	// before granting the archive's different workflow/step/path tuple.
	if _, err := db.Grant(t.Context(), "upstream/org/other", ".oberth/build.yaml", "build", "oberth/data/upstream/org/other/token", "admin"); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		action := strings.TrimPrefix(r.URL.Path, "/api/admin/")
		calls = append(calls, action)
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer restore-admin" {
			http.Error(w, "admin authentication required", http.StatusUnauthorized)
			return
		}
		value, err := grantRestoreResponse(r, db, action, target.schema)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(value); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	remote, err := client.New(t.Context(), client.Config{BaseURL: server.URL, Token: "restore-admin"})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	deps := target.deps(&output)
	deps.Admin = remote
	cfg := RestoreConfig{Input: archive, Only: []string{RestorePartSecrets}, Yes: true, Timeout: 10 * time.Second}
	cfg.applyDefaults()
	if err := runRestore(t.Context(), cfg, deps); err != nil {
		t.Fatalf("secrets-only restore through client: %v\n%s", err, output.String())
	}
	grants, err := exportDatabaseGrants(t.Context(), db)
	want := model.SecretAccessKey{Repo: "upstream/org/repo", Workflow: ".oberth/release.yaml", Step: "*", Path: "oberth/data/release/cosign-secret"}
	if err != nil || !slices.Equal(grants.Grants, []model.SecretAccessKey{want}) {
		t.Fatalf("restored grants = %v, error = %v", grants, err)
	}
	mu.Lock()
	defer mu.Unlock()
	revoke, allow := slices.Index(calls, "access-revoke"), slices.Index(calls, "access-allow")
	if revoke < 0 || allow <= revoke {
		t.Fatalf("expected HTTP revoke before allow, got %v", calls)
	}
	if len(target.uploads) != 0 {
		t.Fatal("secrets-only restore uploaded a database")
	}
}

func grantRestoreResponse(r *http.Request, db *store.Store, action string, schema int) (any, error) {
	switch action {
	case "version":
		return map[string]any{"version": "v0.22.8", "schema": schema}, nil
	case "audit-record":
		return map[string]any{}, nil
	case "public-identity":
		return map[string]string{"content": ""}, nil
	case "access-list":
		grants, err := exportDatabaseGrants(r.Context(), db)
		return map[string]any{"grants": grants.Grants}, err
	case "access-allow", "access-revoke":
		var key model.SecretAccessKey
		if err := json.NewDecoder(r.Body).Decode(&key); err != nil {
			return nil, err
		}
		if action == "access-revoke" {
			_, err := db.Revoke(r.Context(), key.Repo, key.Workflow, key.Step, key.Path, "admin")
			return map[string]any{}, err
		}
		_, err := db.Grant(r.Context(), key.Repo, key.Workflow, key.Step, key.Path, "admin")
		return map[string]any{}, err
	default:
		return nil, http.ErrNotSupported
	}
}
