package installer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"

	"github.com/oberthci/oberth/internal/client"
)

func TestUpgradeConfiguredAdminRepairsWithoutRunningPod(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, name := range []string{"OBERTH_MCP_URL", "OBERTH_TOKEN_COMMAND", "OBERTH_CLIENT_CERT", "OBERTH_CLIENT_KEY", "OBERTH_CA_CERT"} {
		t.Setenv(name, "")
	}
	t.Setenv("OBERTH_BASE_URL", "https://server.example.test:8443")
	t.Setenv("OBERTH_TOKEN", "fixture")
	f := newUpgradeFixture(t, "0.22.7", "0.22.7", "99")
	f.deps.Admin = nil
	f.deps.ClientConfigRoot = func() (string, error) { return t.TempDir(), nil }
	kube := f.deps.KubeClient.(*fake.Clientset)
	_, err := kube.CoreV1().Pods(DefaultNamespace).Create(t.Context(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "oberth-pending", Namespace: DefaultNamespace, Labels: map[string]string{"app.kubernetes.io/instance": "oberth"}},
		Status:     corev1.PodStatus{Phase: corev1.PodPending},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	kube.ClearActions()
	if err := checkUpgradeVerificationConnection(t.Context(), UpgradeConfig{Namespace: DefaultNamespace}, f.deps); err != nil || len(kube.Actions()) != 1 || !kube.Actions()[0].Matches("list", "pods") {
		t.Error("credential preflight did more than observe Pods", err, kube.Actions())
	}
	writes := 0
	stop := errors.New("stop at Helm write")
	helm := f.deps.RunHelm
	f.deps.RunHelm = func(ctx context.Context, args []string) ([]byte, error) {
		if args[0] == "list" && slices.Contains(args, DefaultNamespace) {
			return []byte(`[{"name":"oberth","namespace":"oberth","revision":"7","status":"failed","chart":"oberth-0.22.7"}]`), nil
		}
		if args[0] == "upgrade" && !slices.Contains(args, "--dry-run=server") {
			writes++
			return nil, stop
		}
		return helm(ctx, args)
	}
	_, err = RunUpgrade(t.Context(), UpgradeConfig{BinaryVersion: "0.22.7", BinarySchemaVersion: 99, Namespace: DefaultNamespace, NoBackup: true, RecordDir: t.TempDir()}, f.deps)
	if writes != 1 || !errors.Is(err, stop) {
		t.Fatal("configured failed-release repair did not reach Helm write", writes, err)
	}
}

func TestUpgradePreflightValidatesClientConfiguration(t *testing.T) {
	for _, tc := range []struct{ name, variable, value, want string }{
		{"token command", "OBERTH_TOKEN_COMMAND", "exit 1", "OBERTH_TOKEN_COMMAND failed"},
		{"CA", "OBERTH_CA_CERT", "/missing/oberth-ca.crt", "OBERTH_CA_CERT"},
		{"certificate", "OBERTH_CLIENT_CERT", "/missing/oberth-admin.crt", "both client certificate and key"},
		{"certificate files", "OBERTH_CLIENT_CERT", "/missing/oberth-admin.crt", "cannot inspect client private key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			for _, name := range []string{"OBERTH_MCP_URL", "OBERTH_TOKEN", "OBERTH_TOKEN_COMMAND", "OBERTH_CLIENT_CERT", "OBERTH_CLIENT_KEY", "OBERTH_CA_CERT"} {
				t.Setenv(name, "")
			}
			t.Setenv("OBERTH_BASE_URL", "https://server.example.test:8443")
			if tc.variable != "OBERTH_TOKEN_COMMAND" {
				t.Setenv("OBERTH_TOKEN", "fixture")
			}
			t.Setenv(tc.variable, tc.value)
			if tc.name == "certificate files" {
				t.Setenv("OBERTH_CLIENT_KEY", "/missing/oberth-admin.key")
			}
			kube := fake.NewClientset()
			deps := Deps{KubeClient: kube, RestConfig: &rest.Config{Host: "https://api.example.test"}, ClientConfigRoot: func() (string, error) { return t.TempDir(), nil }}
			err := checkUpgradeVerificationConnection(t.Context(), UpgradeConfig{}, deps)
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "nothing changed") || len(kube.Actions()) != 0 {
				t.Fatal("invalid client configuration escaped preflight", err, kube.Actions())
			}
		})
	}
}

func TestUpgradeUnconfiguredAdminRefusesBeforeMutation(t *testing.T) {
	for _, version := range []string{"0.22.4", "0.22.6", "0.22.7"} {
		for _, noBackup := range []bool{false, true} {
			for _, dry := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/no-backup=%t/dry=%t", version, noBackup, dry), func(t *testing.T) { testUpgradeUnconfiguredAdmin(t, version, noBackup, dry) })
			}
		}
	}
}

func testUpgradeUnconfiguredAdmin(t *testing.T, version string, noBackup, dry bool) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, name := range []string{"OBERTH_BASE_URL", "OBERTH_MCP_URL", "OBERTH_TOKEN", "OBERTH_TOKEN_COMMAND", "OBERTH_CLIENT_CERT", "OBERTH_CLIENT_KEY", "OBERTH_CA_CERT"} {
		t.Setenv(name, "")
	}
	f := newUpgradeFixture(t, version, "0.22.8", "99")
	f.deps.Admin = nil
	f.deps.ClientConfigRoot = func() (string, error) { return t.TempDir(), nil }
	writes, commands := 0, 0
	helm := f.deps.RunHelm
	f.deps.RunHelm = func(ctx context.Context, args []string) ([]byte, error) {
		if args[0] == "upgrade" && !slices.Contains(args, "--dry-run=server") {
			writes++
		}
		return helm(ctx, args)
	}
	f.deps.RunCommand = func(context.Context, []byte, string, ...string) ([]byte, error) {
		commands++
		return nil, fmt.Errorf("unexpected host command")
	}
	_, err := RunUpgrade(t.Context(), UpgradeConfig{BinaryVersion: "0.22.8", Namespace: DefaultNamespace, NoBackup: noBackup, DryRun: dry}, f.deps)
	for _, action := range f.deps.KubeClient.(*fake.Clientset).Actions() {
		if slices.Contains([]string{"create", "update", "patch", "delete"}, action.GetVerb()) {
			t.Fatal("Kubernetes write before prerequisite refusal", action.GetVerb())
		}
	}
	if err == nil || writes != 0 || commands != 0 {
		t.Fatal("unconfigured client reached a write", writes, commands, err)
	}
	if !noBackup && version != "0.22.7" {
		for _, want := range []string{"./oberth-v" + version + " backup --history-only --context fixture --namespace oberth", "oberth upgrade --no-backup", "not copied", "The retry requires this machine's client connected to this server as an unscoped admin.\n"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatal("missing exact backup guidance", want, err)
			}
		}
	} else {
		for _, want := range []string{"unscoped admin", "export OBERTH_BASE_URL=", "OBERTH_TOKEN=", "OBERTH_CLIENT_CERT=", "OBERTH_CLIENT_KEY=", "OBERTH_CA_CERT=", "nothing changed"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatal("missing client prerequisite", want, err)
			}
		}
	}
}

func TestUpgradePrereleaseHasAdminAPI(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/api/admin/version" {
			t.Error("unexpected admin route", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":"v0.22.7-rc.1","schema":99}`))
	}))
	defer server.Close()
	remote, err := client.New(t.Context(), client.Config{BaseURL: server.URL, Token: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"0.22.7-0", "0.22.7-alpha.1", "0.22.7-rc.1", "0.22.7"} {
		if err := checkUpgradeBackupAPI(t.Context(), UpgradeConfig{}, Deps{Admin: remote}, version); err != nil {
			t.Fatal("API-capable release refused", version, err)
		}
	}
	if requests != 4 {
		t.Fatal("API-capable releases did not probe admin API", requests)
	}
}

func TestUpgradeMissingCurrentAdminAPIGivesRetryCommand(t *testing.T) {
	deps := testAdminDeps(Deps{ContextName: "selected"})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) }))
	defer server.Close()
	remote, err := client.New(t.Context(), client.Config{BaseURL: server.URL, Token: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	deps.Admin = remote
	err = checkUpgradeBackupAPI(t.Context(), UpgradeConfig{Namespace: DefaultNamespace, DryRun: true}, deps, "0.22.7")
	if err == nil || !strings.Contains(err.Error(), "oberth upgrade --no-backup --context selected --namespace oberth") || !strings.Contains(err.Error(), "older Pods") {
		t.Fatal("failed-release guidance missing", err)
	}
	f := newUpgradeFixture(t, "0.22.7", "0.22.8", "99")
	f.deps.Admin = remote
	withUpgradeWorkflowAPI(t, &f.deps, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(upgradeWorkflowList()))
	})
	if _, err := f.deps.KubeClient.CoreV1().Pods(DefaultNamespace).Create(t.Context(), upgradePreflightPod("server", corev1.PodRunning), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	writes, stop := stopUpgradeAtHelmWrite(&f.deps)
	cfg := UpgradeConfig{BinaryVersion: "0.22.8", BinarySchemaVersion: 99, Namespace: DefaultNamespace, RecordDir: t.TempDir()}
	if _, err := RunUpgrade(t.Context(), cfg, f.deps); err == nil || !strings.Contains(err.Error(), "older Pods") || *writes != 0 {
		t.Fatal("missing API did not refuse backup before Helm write", *writes, err)
	}
	cfg.NoBackup = true
	if _, err := RunUpgrade(t.Context(), cfg, f.deps); !errors.Is(err, stop) || *writes != 1 {
		t.Fatal("printed failed-release retry did not reach Helm write", *writes, err)
	}
	var output bytes.Buffer
	_, err = printUpgradeDryRun(&output, UpgradeConfig{WaitTimeout: time.Minute}, UpgradeResult{PreviousVersion: "0.22.7", TargetVersion: "0.22.8"}, false)
	if err != nil || !strings.Contains(output.String(), "active Workflows") || strings.Contains(output.String(), "live daemon queue") {
		t.Fatal("dry-run describes the wrong observer", err, output.String())
	}
}
