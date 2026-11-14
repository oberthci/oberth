package installer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/client-go/rest"

	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/store"
)

func TestUpgradeReleasedServerWithoutAdminAPI(t *testing.T) {
	for _, version := range []string{"0.22.4", "0.22.6"} {
		for _, noBackup := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/no-backup=%t", version, noBackup), func(t *testing.T) { testReleasedServerUpgrade(t, version, noBackup) })
		}
	}
}

func testReleasedServerUpgrade(t *testing.T, version string, noBackup bool) {
	fixture := newUpgradeFixture(t, version, "0.22.7", "99")
	var applied atomic.Bool
	var nativeReads atomic.Int32
	var oldAPIReads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/api/admin/") {
			if !applied.Load() {
				oldAPIReads.Add(1)
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if r.URL.Path == "/api/admin/version" {
				_ = json.NewEncoder(w).Encode(map[string]any{"version": "v0.22.7", "schema": store.LatestMigrationVersion()})
				return
			}
			_, _ = w.Write([]byte(`{}`))
			return
		}
		if r.URL.Path != "/apis/argoproj.io/v1alpha1/namespaces/oberth-argo/workflows" || r.URL.Query().Get("labelSelector") != "oberth.ci/run" {
			t.Error("unexpected Kubernetes read", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		phase := "Running"
		if nativeReads.Add(1) > 1 {
			phase = "Succeeded"
		}
		_ = json.NewEncoder(w).Encode(workflowObservation(phase))
	}))
	defer server.Close()
	remote, err := client.New(t.Context(), client.Config{BaseURL: server.URL, Token: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	fixture.deps.Admin = remote
	fixture.deps.RestConfig = &rest.Config{Host: server.URL}
	fixture.deps.PollInterval = time.Millisecond
	fixture.deps.RunCommand = func(context.Context, []byte, string, ...string) ([]byte, error) {
		t.Fatal("upgrade attempted server exec")
		return nil, nil
	}
	runHelm := fixture.deps.RunHelm
	stored, err := os.ReadFile("testdata/v0.22.3-stored-config.json")
	if err != nil {
		t.Fatal(err)
	}
	var values map[string]any
	if json.Unmarshal(stored, &values) != nil {
		t.Fatal("invalid stored-values fixture")
	}
	values["compatibility"] = map[string]string{"schemaIdentity": testUpgradeCompatibility.SchemaIdentity, "auditDomain": testUpgradeCompatibility.AuditDomain, "witnessKeyInfo": testUpgradeCompatibility.WitnessKeyInfo}
	fixture.deps.RunHelm = func(ctx context.Context, args []string) ([]byte, error) {
		if len(args) > 2 && args[0] == "get" && args[1] == "values" && args[2] == "oberth" {
			return json.Marshal(values)
		}
		if args[0] == "upgrade" && !slices.Contains(args, "--dry-run=server") {
			if nativeReads.Load() < 2 {
				t.Fatal("Helm mutation preceded native idle observation")
			}
			assertReleasedStoredValues(t, args)
			applied.Store(true)
		}
		return runHelm(ctx, args)
	}
	backupCalls := 0
	cfg := UpgradeConfig{BinaryVersion: "0.22.7", BinarySchemaVersion: store.LatestMigrationVersion(), Namespace: "oberth", NoBackup: noBackup, WaitIdle: true, WaitFor: "critical", WaitTimeout: time.Second, Timeout: time.Second, RecordDir: t.TempDir(), Backup: func(context.Context) (UpgradeBackup, error) {
		backupCalls++
		return UpgradeBackup{}, errors.New("old-server backup must be refused before callback")
	}}
	result, err := RunUpgrade(t.Context(), cfg, fixture.deps)
	if !noBackup {
		if err == nil || applied.Load() || backupCalls != 0 || nativeReads.Load() != 0 {
			t.Fatal("pre-API backup did not refuse before changes", err, backupCalls, nativeReads.Load())
		}
		// These exact flags exist in v0.22.6 cmd/oberth/backup.go, including
		// --history-only and addClusterFlags' --context/--namespace.
		for _, command := range []string{"./oberth-v" + version + " backup --history-only --context fixture --namespace oberth", "oberth upgrade --no-backup --context fixture --namespace oberth", "confirm no runs are active", "--wait-for critical"} {
			if !strings.Contains(err.Error(), command) {
				t.Fatal("refusal lacks exact recovery instruction", command, err)
			}
		}
		return
	}
	if err != nil || !result.Upgraded || !applied.Load() || backupCalls != 0 || nativeReads.Load() < 2 || oldAPIReads.Load() == 0 {
		t.Fatal("old-server no-backup upgrade failed", result, err, backupCalls, nativeReads.Load(), oldAPIReads.Load())
	}
}

func assertReleasedStoredValues(t *testing.T, args []string) {
	t.Helper()
	found := false
	for i, arg := range args {
		if (arg != "--values" && arg != "-f") || i+1 >= len(args) {
			continue
		}
		raw, err := os.ReadFile(args[i+1])
		if err != nil {
			t.Fatal(err)
		}
		var config map[string]any
		if json.Unmarshal(raw, &config) != nil {
			continue
		}
		if _, exists := config["sourceAccess"]; !exists {
			continue
		}
		found = true
		if config["sourceAccess"] != nil || config["pushBannerURL"] != "" || valueAt(config, "retention", "keepNewestPerRef") != float64(0) {
			t.Fatal("stored null, empty or zero operator values changed")
		}
		if valueAt(config, "argo", "goProxy") != nil || valueAt(config, "secretstore", "transit") != nil {
			t.Fatal("retired values survived cleanup")
		}
	}
	if !found {
		t.Fatal("upgrade omitted the stored-values snapshot")
	}
}

func workflowObservation(phase string) map[string]any {
	return map[string]any{"apiVersion": "argoproj.io/v1alpha1", "kind": "WorkflowList", "items": []any{map[string]any{"metadata": map[string]any{"name": "fixture", "labels": map[string]string{"oberth.ci/run": "run"}}, "status": map[string]any{"phase": phase}}}}
}

func TestUpgradeNativeWorkflowObservation(t *testing.T) {
	for _, phase := range []string{"", "Pending", "Running", "Succeeded", "Failed", "Error"} {
		t.Run("phase="+phase, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != "/apis/argoproj.io/v1alpha1/namespaces/pipelines/workflows" || r.URL.Query().Get("labelSelector") != "oberth.ci/run" {
					t.Error("native observation lost its selected namespace or selector")
				}
				w.Header().Set("Content-Type", "application/json")
				current := phase
				if calls > 1 {
					current = "Succeeded"
				}
				_ = json.NewEncoder(w).Encode(workflowObservation(current))
			}))
			defer server.Close()
			deps := Deps{RestConfig: &rest.Config{Host: server.URL}, RunHelm: func(context.Context, []string) ([]byte, error) {
				return []byte(`{"argo":{"namespace":"pipelines"}}`), nil
			}, PollInterval: time.Millisecond}
			if err := waitForUpgradeIdle(t.Context(), UpgradeConfig{WaitTimeout: time.Second}, deps); err != nil {
				t.Fatal(err)
			}
			want := 1
			if phase == "" || phase == "Pending" || phase == "Running" {
				want = 2
			}
			if calls != want {
				t.Fatal("native phase handling", phase, calls, want)
			}
		})
	}
	t.Run("UnobservableRefuses", func(t *testing.T) {
		if err := waitForUpgradeIdle(t.Context(), UpgradeConfig{}, Deps{}); err == nil || !strings.Contains(err.Error(), "confirm no runs are active") {
			t.Fatal("unobservable native state accepted", err)
		}
	})
}
