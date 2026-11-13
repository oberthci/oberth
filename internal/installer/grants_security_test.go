package installer

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

func TestDatabaseGrantsMaterializeBothStepTiers(t *testing.T) {
	db, err := store.CreateGenesis(t.Context(), filepath.Join(t.TempDir(), "oberth.sqlite"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	const path = "oberth/upstream/org/repo/a"
	if _, err := db.Grant(t.Context(), "upstream/org/repo", ".oberth/build.yaml", "build", path, "admin"); err != nil {
		t.Fatal(err)
	}
	export, err := exportDatabaseGrants(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(export.Grants)
	if err != nil {
		t.Fatal(err)
	}
	repos, rows, err := ParseAccessListJSONWithSteps(raw)
	if err != nil {
		t.Fatal(err)
	}
	ids := DerivePerStepIdentities(repos, rows)
	if len(ids) != 1 || ids[0].Workflow != ".oberth/build.yaml" {
		t.Fatal(ids)
	}
	responses := map[string]fakeBaoResponse{}
	names := PerStepIdentityNames(ids)
	if len(names) != 2 || names[0] == names[1] {
		t.Fatal("tier identities are not distinct", names)
	}
	for _, name := range names {
		responses["policy read "+name] = fakeBaoResponse{out: "No policy named: " + name, err: os.ErrNotExist}
		responses["policy write "+name+" -"] = fakeBaoResponse{}
		responses["read -format=json auth/kubernetes/role/"+name] = fakeBaoResponse{out: "No value found", err: os.ErrNotExist}
		responses["write auth/kubernetes/role/"+name+" -"] = fakeBaoResponse{}
	}
	runner := &fakeBaoRunner{t: t, responses: responses}
	items, err := ConfigurePerStepIdentities(t.Context(), openBaoExec{run: runner.run, namespace: "openbao", pod: "openbao-0"}, "fixture-root", ids, "pipelines")
	if err != nil || len(items) != 4 {
		t.Fatal(items, err)
	}
	policies, roles := map[string]string{}, map[string]map[string]any{}
	for _, call := range runner.calls {
		for _, name := range names {
			if call.command == "policy write "+name+" -" {
				_, policies[name], _ = strings.Cut(call.stdin, "\n")
			}
			if call.command == "write auth/kubernetes/role/"+name+" -" {
				var payload map[string]any
				if json.Unmarshal([]byte(strings.SplitN(call.stdin, "\n", 2)[1]), &payload) != nil {
					t.Fatal("invalid role payload")
				}
				roles[name] = payload
			}
		}
	}
	for _, name := range names {
		if !strings.Contains(policies[name], `path "oberth/data/upstream/org/repo/a"`) || strings.Contains(policies[name], `upstream/org/repo/*`) {
			t.Fatal("materialized policy widened the grant", policies[name])
		}
		if roles[name]["bound_service_account_names"] != name || roles[name]["bound_service_account_namespaces"] != "pipelines" || roles[name]["token_policies"] != name || roles[name]["token_no_default_policy"] != true {
			t.Fatal("role was not bound to its own identity", roles[name])
		}
	}
}

func TestGrantBackupRestoreRoundTrip(t *testing.T) {
	t.Run("Full", func(t *testing.T) {
		archive, _, _, target := reinstallFixture(t)
		runFixtureRestore(t, archive, target, RestoreConfig{})
		keys := uploadedGrantKeys(t, target, sourceCompat)
		if len(keys) != 1 || keys[0].Workflow != ".oberth/release.yaml" || keys[0].Path != "oberth/data/release/cosign-secret" {
			t.Fatal("full restore lost exact grants", keys)
		}
	})
	t.Run("SecretsOnly", func(t *testing.T) {
		archive, _, _, target := reinstallFixture(t)
		runFixtureRestore(t, archive, target, RestoreConfig{Only: []string{RestorePartSecrets}})
		db, _, cleanup, err := target.openGrantDatabase(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = cleanup() }()
		keys, err := exportDatabaseGrants(t.Context(), db)
		if err != nil || len(keys.Grants) != 1 || keys.Grants[0].Workflow != ".oberth/release.yaml" {
			t.Fatal(keys, err)
		}
		if len(target.uploads) != 0 {
			t.Fatal("secrets-only restore uploaded a database")
		}
	})
	t.Run("HistoryPreservesRevocationBeforeShutdown", func(t *testing.T) {
		archive, _, _, target := reinstallFixture(t)
		key := model.SecretAccessKey{Repo: "upstream/target/repo", Workflow: ".oberth/target.yaml", Step: "build", Path: "oberth/upstream/target/repo/own"}
		if err := writeSecretAccess(t.Context(), target.deps(&bytes.Buffer{}).Deps, DefaultNamespace, secretAccessExport{Present: true, Grants: []model.SecretAccessKey{key}}); err != nil {
			t.Fatal(err)
		}
		revoked := false
		target.beforeServerStop = func() {
			db, path, cleanup, err := target.openGrantDatabase(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			_, err = db.Revoke(t.Context(), key.Repo, key.Workflow, key.Step, key.Path, "admin@fixture")
			closeErr := cleanup()
			if err != nil || closeErr != nil {
				t.Fatal(err, closeErr)
			}
			if err := target.captureGrantDatabase(t.Context(), path); err != nil {
				t.Fatal(err)
			}
			revoked = true
		}
		runFixtureRestore(t, archive, target, RestoreConfig{Only: []string{RestorePartHistory}})
		keys := uploadedGrantKeys(t, target, sourceCompat)
		if !revoked || len(keys) != 0 {
			t.Fatal("history restore resurrected a grant revoked after preflight", keys)
		}
	})
	t.Run("HistoryPreservesStoppedTargetGrants", func(t *testing.T) {
		archive, _, _, target := reinstallFixture(t)
		key := model.SecretAccessKey{Repo: "upstream/target/repo", Workflow: ".oberth/target.yaml", Step: "build", Path: "oberth/upstream/target/repo/own"}
		if err := writeSecretAccess(t.Context(), target.deps(&bytes.Buffer{}).Deps, DefaultNamespace, secretAccessExport{Present: true, Grants: []model.SecretAccessKey{key}}); err != nil {
			t.Fatal(err)
		}
		runFixtureRestore(t, archive, target, RestoreConfig{Only: []string{RestorePartHistory}})
		keys := uploadedGrantKeys(t, target, sourceCompat)
		if len(keys) != 1 || keys[0] != key {
			t.Fatal("history-only restore replaced target grants", keys)
		}
	})
}

func uploadedGrantKeys(t *testing.T, c *fakeBackupCluster, compat backupCompatibility) []model.SecretAccessKey {
	t.Helper()
	work := t.TempDir()
	path := filepath.Join(work, "oberth.sqlite")
	for _, suffix := range []string{"", "-wal"} {
		if body, ok := c.uploads["oberth.sqlite"+suffix]; ok {
			if err := os.WriteFile(path+suffix, body, 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	db, err := store.InspectCurrent(t.Context(), path, store.Options{Protocol: store.ProtocolConfig{SchemaIdentity: compat.SchemaIdentity, AuditDomain: compat.AuditDomain}, SnapshotTempDir: work})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	rows, err := exportDatabaseGrants(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}
	return rows.Grants
}
