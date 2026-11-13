package installer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
)

func TestUpgradeAnnouncesItsOwnBackup(t *testing.T) {
	for _, tc := range []struct {
		live     string
		noBackup bool
		want     string
	}{
		{"16", false, "is taken before the first change"},
		{"unknown", false, "is taken before the first change"},
		{"16", true, "NO BACKUP WILL BE TAKEN"},
		{"17", false, ""},
	} {
		t.Run(fmt.Sprintf("%s/no-backup=%t", tc.live, tc.noBackup), func(t *testing.T) {
			var output bytes.Buffer
			deps := Deps{Output: &output, KubeClient: fake.NewClientset(), RestConfig: &rest.Config{Host: "https://cluster.example:6443"},
				RunHelm: func(_ context.Context, args []string) ([]byte, error) {
					if out, ok := testUpgradeValuePreflight(args); ok {
						return out, nil
					}
					if args[0] == "list" {
						return json.Marshal([]helmRelease{{Name: "oberth", Namespace: "chosen", Status: "deployed", Chart: "oberth-0.16.18"}})
					}
					return nil, fmt.Errorf("unexpected mutating helm call: %v", args)
				},
				RunCommand: func(context.Context, []byte, string, ...string) ([]byte, error) { return []byte(tc.live), nil },
			}
			withValidatedDryRunRelease(t, &deps, "0.16.19")
			_, err := RunUpgrade(t.Context(), UpgradeConfig{BinaryVersion: "0.16.19", BinarySchemaVersion: 17, Namespace: "chosen", Yes: true, DryRun: true, NoBackup: tc.noBackup}, deps)
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == "" {
				if strings.Contains(output.String(), "is taken before the first change") || strings.Contains(output.String(), "NO BACKUP") {
					t.Fatalf("no migration, yet a backup note: %s", output.String())
				}
				return
			}
			if !strings.Contains(output.String(), tc.want) || !strings.Contains(output.String(), "namespace chosen") {
				t.Fatalf("missing %q: %s", tc.want, output.String())
			}
		})
	}
}

// An older target server cannot open a newer database: refuse before any
// change and point at the supported rollback.
func TestUpgradeRefusesNewerLiveSchema(t *testing.T) {
	t.Parallel()
	deps := Deps{Output: &bytes.Buffer{}, KubeClient: fake.NewClientset(), RestConfig: &rest.Config{Host: "https://127.0.0.1:6443"},
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			if out, ok := testUpgradeValuePreflight(args); ok {
				return out, nil
			}
			if args[0] == "list" {
				return json.Marshal([]helmRelease{{Name: "oberth", Namespace: "oberth", Status: "deployed", Chart: "oberth-0.16.20"}})
			}
			return nil, fmt.Errorf("unexpected helm call: %v", args)
		},
		RunCommand: func(context.Context, []byte, string, ...string) ([]byte, error) { return []byte("18"), nil },
	}
	_, err := RunUpgrade(t.Context(), UpgradeConfig{BinaryVersion: "0.16.19", BinarySchemaVersion: 17, AllowDowngrade: true, DryRun: true}, deps)
	if err == nil || !strings.Contains(err.Error(), "--rollback") {
		t.Fatalf("newer live schema: %v", err)
	}
	_, err = RunUpgrade(t.Context(), UpgradeConfig{BinaryVersion: "0.16.19", BinarySchemaVersion: 17, DryRun: true}, deps)
	if err == nil || !strings.Contains(err.Error(), "--allow-downgrade") {
		t.Fatalf("downgrade without the flag: %v", err)
	}
	// A downgrade never proceeds on an unreadable schema.
	deps.RunCommand = func(context.Context, []byte, string, ...string) ([]byte, error) {
		return nil, errors.New("container not found")
	}
	_, err = RunUpgrade(t.Context(), UpgradeConfig{BinaryVersion: "0.16.19", BinarySchemaVersion: 17, AllowDowngrade: true, DryRun: true}, deps)
	if err == nil || !strings.Contains(err.Error(), "could not be read") {
		t.Fatalf("downgrade on an unknown schema: %v", err)
	}
}

type upgradeFixture struct {
	deps   Deps
	calls  *[]string
	output *bytes.Buffer
}

func newUpgradeFixture(t *testing.T, installed, target, liveSchema string) upgradeFixture {
	t.Helper()
	image := fakeChartImageRef
	replicas := int32(1)
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "oberth", Namespace: "oberth", Generation: 2, Labels: map[string]string{"app.kubernetes.io/instance": "oberth"}},
		Spec: appsv1.DeploymentSpec{Replicas: &replicas, Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "oberth", Image: image}},
		}}},
		Status: appsv1.DeploymentStatus{ObservedGeneration: 2, UpdatedReplicas: 1, AvailableReplicas: 1},
	}
	calls := &[]string{}
	output := &bytes.Buffer{}
	deps := Deps{
		Output: output,
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			if out, ok := testUpgradeValuePreflight(args); ok {
				return out, nil
			}
			*calls = append(*calls, strings.Join(args, " "))
			switch args[0] {
			case "list":
				// Upgrade fixtures start at revision 7; rollback records name
				// the revisions before the upgrade they undo (6, and argo 3).
				if slices.Contains(args, "oberth-argo") {
					return json.Marshal([]helmRelease{{Name: "argo-workflows", Namespace: "oberth-argo", Status: "deployed", Chart: "argo-workflows-1.0.24", Revision: json.RawMessage(`4`)}})
				}
				return json.Marshal([]helmRelease{{Name: "oberth", Namespace: "oberth", Status: "deployed", Chart: "oberth-" + installed, Revision: json.RawMessage(`7`)}})
			case "show":
				return []byte("image:\n  ref: " + image + "\n"), nil
			case "upgrade", "rollback":
				return nil, nil
			}
			return nil, fmt.Errorf("unexpected helm call: %v", args)
		},
		RunCommand: func(_ context.Context, _ []byte, _ string, args ...string) ([]byte, error) {
			command := strings.Join(args, " ")
			switch {
			case strings.HasSuffix(command, "version --schema"):
				return []byte(liveSchema), nil
			case strings.HasSuffix(command, "oberth version"):
				*calls = append(*calls, "verify")
				return []byte("oberth v" + strings.TrimPrefix(target, "v") + " commit=abc date=2026-11-10T00:00:00Z\n"), nil
			}
			return nil, nil
		},
		KubeClient:  fake.NewClientset(deployment),
		RestConfig:  &rest.Config{Host: "https://127.0.0.1:6443"},
		ContextName: "fixture",
	}
	withSignedRelease(t, &deps, target, image)
	return upgradeFixture{deps: deps, calls: calls, output: output}
}

func TestUpgradeBacksUpBeforeTheFirstChangeAndRecordsTheRollback(t *testing.T) {
	t.Parallel()
	// Below v0.17.83 the fixture needs no retired-execution inventory.
	fixture := newUpgradeFixture(t, "0.16.20", "0.16.21", "73")
	dir := t.TempDir()
	archive := filepath.Join(dir, "oberth-backup-test.tar.gz.enc")
	cfg := UpgradeConfig{BinaryVersion: "0.16.21", BinarySchemaVersion: 74, Namespace: "oberth", Timeout: 10 * time.Second,
		Backup: func(context.Context) (UpgradeBackup, error) {
			for _, call := range *fixture.calls {
				if strings.HasPrefix(call, "upgrade ") && !strings.Contains(call, "--dry-run=server") {
					t.Fatalf("backup after a write: %v", *fixture.calls)
				}
			}
			*fixture.calls = append(*fixture.calls, "backup")
			return UpgradeBackup{Path: archive}, nil
		}}
	result, err := RunUpgrade(t.Context(), cfg, fixture.deps)
	if err != nil {
		t.Fatal(err)
	}
	backupAt := slices.Index(*fixture.calls, "backup")
	upgradeAt := slices.IndexFunc(*fixture.calls, func(call string) bool {
		return strings.HasPrefix(call, "upgrade oberth") && !strings.Contains(call, "--dry-run=server")
	})
	if backupAt < 0 || upgradeAt < backupAt {
		t.Fatalf("backup must precede the upgrade: %v", *fixture.calls)
	}
	if result.RecordPath == "" || filepath.Dir(result.RecordPath) != dir {
		t.Fatalf("record %q not next to the backup in %s", result.RecordPath, dir)
	}
	record, err := ReadUpgradeRecord(result.RecordPath)
	if err != nil {
		t.Fatal(err)
	}
	if record.From != "0.16.20" || record.To != "0.16.21" || record.OberthRevision != 7 || record.SchemaBefore != 73 || record.SchemaAfter != 74 ||
		record.Backup != archive || record.APIServer != "https://127.0.0.1:6443" || !record.migrates() {
		t.Fatalf("record = %+v", record)
	}
	if info, err := os.Stat(result.RecordPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("record mode: %v %v", info, err)
	}
	if !strings.Contains(fixture.output.String(), "oberth upgrade --rollback "+result.RecordPath) {
		t.Fatalf("rollback command not printed: %s", fixture.output.String())
	}
}

func TestUpgradeWithoutABackupChangesNothing(t *testing.T) {
	t.Parallel()
	for name, cfg := range map[string]UpgradeConfig{
		"no backup configured": {},
		"backup fails": {Backup: func(context.Context) (UpgradeBackup, error) {
			return UpgradeBackup{}, errors.New("no running server pod")
		}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fixture := newUpgradeFixture(t, "0.16.20", "0.16.21", "73")
			cfg.BinaryVersion, cfg.BinarySchemaVersion, cfg.Namespace, cfg.RecordDir = "0.16.21", 74, "oberth", t.TempDir()
			_, err := RunUpgrade(t.Context(), cfg, fixture.deps)
			if err == nil || !strings.Contains(err.Error(), "--no-backup") {
				t.Fatalf("err = %v", err)
			}
			for _, call := range *fixture.calls {
				if strings.HasPrefix(call, "upgrade ") && !strings.Contains(call, "--dry-run=server") {
					t.Fatalf("wrote without a backup: %v", *fixture.calls)
				}
			}
			if entries, _ := os.ReadDir(cfg.RecordDir); len(entries) != 0 {
				t.Fatal("record written although the upgrade never started")
			}
		})
	}
}

func writeTestRecord(t *testing.T, record UpgradeRecord) string {
	t.Helper()
	record.Format = upgradeRecordFormat
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "record.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRollbackRestoresTheBackupAfterAMigration(t *testing.T) {
	t.Parallel()
	fixture := newUpgradeFixture(t, "0.21.0", "0.20.1", "74")
	archive := filepath.Join(t.TempDir(), "backup.tar.gz.enc")
	if err := os.WriteFile(archive, []byte("archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := writeTestRecord(t, UpgradeRecord{APIServer: "https://127.0.0.1:6443", Namespace: "oberth", From: "0.20.1", To: "0.21.0",
		OberthRevision: 6, SchemaBefore: 73, SchemaAfter: 74, Backup: archive})
	restored := ""
	restore := func(_ context.Context, target RollbackRestore) error {
		*fixture.calls = append(*fixture.calls, "restore")
		// The restore is aimed at the record's namespace.
		if target.Namespace != "oberth" {
			t.Errorf("restore namespace = %q, want the record's", target.Namespace)
		}
		restored = target.Archive
		return nil
	}
	// Replacing the database needs --yes.
	if err := RunRollback(t.Context(), RollbackConfig{Record: path, Timeout: time.Minute, Restore: restore}, fixture.deps); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("restoring rollback without --yes: %v", err)
	}
	// The rollback names its target before it plans anything.
	if !strings.Contains(fixture.output.String(), "Target: fixture (") {
		t.Fatalf("no target line:\n%s", fixture.output.String())
	}
	for _, call := range *fixture.calls {
		if strings.HasPrefix(call, "rollback") || call == "restore" {
			t.Fatalf("unconfirmed rollback ran %q", call)
		}
	}
	err := RunRollback(t.Context(), RollbackConfig{Record: path, Yes: true, Timeout: time.Minute, Restore: restore}, fixture.deps)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"list -n oberth --deployed --failed --pending --superseded --uninstalling --uninstalled -o json",
		"rollback oberth 6 -n oberth --timeout 1m0s",
		"restore",
		"verify",
	}
	got := slices.DeleteFunc(slices.Clone(*fixture.calls), func(call string) bool { return strings.HasPrefix(call, "list") })
	if !slices.Equal(got, want[1:]) || restored != archive {
		t.Fatalf("rollback sequence = %v (restored %q)", *fixture.calls, restored)
	}
}

// The restore of a rollback goes to the record's installation, not to the
// default namespace: another install there must never get this database.
func TestRollbackRestoresIntoTheRecordsNamespace(t *testing.T) {
	t.Parallel()
	fixture := newUpgradeFixture(t, "0.21.0", "0.20.1", "74")
	archive := filepath.Join(t.TempDir(), "backup.tar.gz.enc")
	if err := os.WriteFile(archive, []byte("archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := writeTestRecord(t, UpgradeRecord{APIServer: "https://127.0.0.1:6443", Namespace: "ci", From: "0.20.1", To: "0.21.0",
		OberthRevision: 6, SchemaBefore: 73, SchemaAfter: 74, Backup: archive})
	var got RollbackRestore
	// The fixture's server runs in "oberth", so the readback after the
	// restore fails; what matters is where the restore was aimed.
	_ = RunRollback(t.Context(), RollbackConfig{Record: path, Yes: true, Timeout: time.Minute, Restore: func(_ context.Context, target RollbackRestore) error {
		got = target
		return nil
	}}, fixture.deps)
	if got.Namespace != "ci" || got.Archive != archive {
		t.Fatalf("restore target = %+v, want namespace ci", got)
	}
	if !slices.Contains(*fixture.calls, "rollback oberth 6 -n ci --timeout 1m0s") {
		t.Fatalf("helm rollback not in the record's namespace: %v", *fixture.calls)
	}
}

func TestRollbackWithinOneSchemaNeedsNoRestore(t *testing.T) {
	t.Parallel()
	fixture := newUpgradeFixture(t, "0.21.1", "0.21.0", "74")
	path := writeTestRecord(t, UpgradeRecord{Namespace: "oberth", From: "0.21.0", To: "0.21.1", OberthRevision: 6, SchemaBefore: 74, SchemaAfter: 74})
	if err := RunRollback(t.Context(), RollbackConfig{Record: path, Timeout: time.Minute}, fixture.deps); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(*fixture.calls, "rollback oberth 6 -n oberth --timeout 1m0s --wait") || slices.Contains(*fixture.calls, "restore") {
		t.Fatalf("calls = %v", *fixture.calls)
	}
}

func TestRollbackRefusals(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		record UpgradeRecord
		want   string
	}{
		"migration without backup": {UpgradeRecord{Namespace: "oberth", From: "0.20.1", To: "0.21.0", OberthRevision: 6, SchemaBefore: 73, SchemaAfter: 74}, "took no backup"},
		"other cluster":            {UpgradeRecord{APIServer: "https://other.example:6443", Namespace: "oberth", From: "0.20.1", To: "0.21.0", OberthRevision: 6, SchemaBefore: 74, SchemaAfter: 74}, "API server"},
		"stale record":             {UpgradeRecord{Namespace: "oberth", From: "0.20.1", To: "0.21.0", OberthRevision: 3, SchemaBefore: 74, SchemaAfter: 74}, "record is stale"},
		"release moved":            {UpgradeRecord{Namespace: "oberth", From: "0.19.0", To: "0.19.1", OberthRevision: 6, SchemaBefore: 74, SchemaAfter: 74}, "moved since"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fixture := newUpgradeFixture(t, "0.21.0", "0.20.1", "74")
			err := RunRollback(t.Context(), RollbackConfig{Record: writeTestRecord(t, tc.record)}, fixture.deps)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			for _, call := range *fixture.calls {
				if strings.HasPrefix(call, "rollback") {
					t.Fatalf("refused rollback still ran %q", call)
				}
			}
		})
	}
	// --dry-run prints the plan and changes nothing.
	fixture := newUpgradeFixture(t, "0.21.0", "0.20.1", "74")
	path := writeTestRecord(t, UpgradeRecord{Namespace: "oberth", From: "0.20.1", To: "0.21.0", OberthRevision: 6, SchemaBefore: 74, SchemaAfter: 74})
	if err := RunRollback(t.Context(), RollbackConfig{Record: path, DryRun: true}, fixture.deps); err != nil {
		t.Fatal(err)
	}
	for _, call := range *fixture.calls {
		if strings.HasPrefix(call, "rollback") {
			t.Fatalf("dry-run ran %q", call)
		}
	}
	stale := writeTestRecord(t, UpgradeRecord{Namespace: "oberth", From: "0.20.1", To: "0.21.0", OberthRevision: 3, SchemaBefore: 74, SchemaAfter: 74})
	if err := RunRollback(t.Context(), RollbackConfig{Record: stale, DryRun: true, ForceRecord: true}, fixture.deps); err != nil {
		t.Fatalf("--force-record did not accept a stale record: %v", err)
	}
	if !strings.Contains(fixture.output.String(), "helm rollback oberth 6") {
		t.Fatalf("plan: %s", fixture.output.String())
	}
}

// A record binds to where its upgrade left each release: an upgrade that
// failed before reaching oberth leaves it at the recorded revision (only
// nothing migrated, nothing restored), and a revision
// past the upgrade's is accepted only when helm history shows nothing but
// rollbacks to the recorded revision (no second helm rollback; the restore
// still runs).
func TestRollbackRecordStands(t *testing.T) {
	t.Parallel()
	archive := filepath.Join(t.TempDir(), "backup.tar.gz.enc")
	if err := os.WriteFile(archive, []byte("archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		oberthRevision int
		history        string
		want           []string
		wantErr        string
	}{
		"upgrade failed before the server transition": {
			oberthRevision: 6,
			want:           []string{"verify"},
		},
		"a rollback of this record already ran": {
			oberthRevision: 9, history: `[{"revision":7,"description":"Upgrade complete"},{"revision":8,"description":"Rollback to 6"},{"revision":9,"description":"Rollback to 6"}]`,
			want: []string{"restore", "verify"},
		},
		"an interrupted rollback of this record": {
			oberthRevision: 8, history: `[{"revision":7,"status":"superseded","description":"Upgrade complete"},{"revision":8,"status":"pending-rollback","description":"Rollback to 6"}]`,
			wantErr: "record is stale",
		},
		"an unrelated upgrade after the recorded one": {
			oberthRevision: 8, history: `[{"revision":7,"description":"Upgrade complete"},{"revision":8,"description":"Upgrade complete"}]`,
			wantErr: "record is stale",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fixture := newUpgradeFixture(t, "0.20.1", "0.20.1", "73")
			base := fixture.deps.RunHelm
			fixture.deps.RunHelm = func(ctx context.Context, args []string) ([]byte, error) {
				switch {
				case args[0] == "history":
					return []byte(tc.history), nil
				case args[0] == "list" && !slices.Contains(args, "oberth-argo"):
					*fixture.calls = append(*fixture.calls, strings.Join(args, " "))
					return json.Marshal([]helmRelease{{Name: "oberth", Namespace: "oberth", Status: "deployed", Chart: "oberth-0.20.1", Revision: json.RawMessage(strconv.Itoa(tc.oberthRevision))}})
				}
				return base(ctx, args)
			}
			record := UpgradeRecord{APIServer: "https://127.0.0.1:6443", Namespace: "oberth", From: "0.20.1", To: "0.21.0",
				OberthRevision: 6, SchemaBefore: 73, SchemaAfter: 74, Backup: archive}
			restore := func(context.Context, RollbackRestore) error {
				*fixture.calls = append(*fixture.calls, "restore")
				return nil
			}
			err := RunRollback(t.Context(), RollbackConfig{Record: writeTestRecord(t, record), Yes: true, Timeout: time.Minute, Restore: restore}, fixture.deps)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got := slices.DeleteFunc(slices.Clone(*fixture.calls), func(call string) bool { return strings.HasPrefix(call, "list") })
			if !slices.Equal(got, tc.want) {
				t.Fatalf("rollback sequence = %v, want %v\n%s", got, tc.want, fixture.output.String())
			}
		})
	}
}
