package installer

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	ktesting "k8s.io/client-go/testing"

	"github.com/oberthci/oberth/internal/client"
)

func TestUpgradeAdminRolePreflight(t *testing.T) {
	for _, tc := range []struct {
		name, detail string
		status       int
	}{
		{"scoped token", "scoped-admin@host requires an unscoped admin identity", http.StatusForbidden},
		{"non-admin token", "reader@host requires an unscoped admin identity", http.StatusForbidden},
		{"unscoped admin", "", http.StatusOK},
		{"rejected token", "invalid token", http.StatusUnauthorized},
		{"missing API", "missing", http.StatusNotFound},
		{"server error", "unavailable", http.StatusServiceUnavailable},
		{"network error", "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) { testUpgradeAdminRolePreflight(t, tc.status, tc.detail) })
	}
}

func testUpgradeAdminRolePreflight(t *testing.T, status int, detail string) {
	t.Helper()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/api/admin/version" || r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer fixture" {
			t.Error("unexpected preflight call", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status == http.StatusOK {
			_, _ = w.Write([]byte(`{"version":"v0.22.7","schema":99}`))
		} else {
			_, _ = w.Write([]byte(`{"error":"` + detail + `"}`))
		}
	}))
	defer server.Close()
	if status == 0 {
		server.Close()
	}
	remote, err := client.New(t.Context(), client.Config{BaseURL: server.URL, Token: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	f := newUpgradeFixture(t, "0.22.7", "0.22.8", "99")
	f.deps.Admin = remote
	kube := f.deps.KubeClient.(*fake.Clientset)
	if _, err := kube.CoreV1().Pods(DefaultNamespace).Create(t.Context(), upgradePreflightPod("server", corev1.PodRunning), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	kube.ClearActions()
	err = checkUpgradeVerificationConnection(t.Context(), UpgradeConfig{Namespace: DefaultNamespace}, f.deps)
	allowed := status == http.StatusOK || status == http.StatusNotFound
	if allowed && err != nil || !allowed && err == nil {
		t.Fatal("unexpected preflight result", err)
	}
	if status != 0 && requests.Load() != 1 {
		t.Fatal("preflight must make exactly one read-only admin call", requests.Load())
	}
	if status == http.StatusForbidden && (!strings.Contains(err.Error(), "this client is not an unscoped admin of this server") || !strings.Contains(err.Error(), detail)) {
		t.Fatal("role refusal lost server identity or advice", err)
	}
	if err != nil && strings.Contains(err.Error(), "configure it with") {
		t.Fatal("live error reported as missing configuration", err)
	}
	requests.Store(0)
	writes, stop := stopUpgradeAtHelmWrite(&f.deps)
	dir := t.TempDir()
	_, err = RunUpgrade(t.Context(), UpgradeConfig{BinaryVersion: "0.22.8", BinarySchemaVersion: 99, Namespace: DefaultNamespace, NoBackup: true, RecordDir: dir}, f.deps)
	if allowed {
		if *writes != 1 || !errors.Is(err, stop) {
			t.Fatal("permitted preflight did not reach Helm write", *writes, err)
		}
		return
	}
	if err == nil || *writes != 0 || status != 0 && requests.Load() != 1 {
		t.Fatal("failed admin probe escaped pre-write refusal", *writes, requests.Load(), err)
	}
	for _, action := range kube.Actions() {
		if slices.Contains([]string{"create", "update", "patch", "delete"}, action.GetVerb()) {
			t.Fatal("admin refusal followed a Kubernetes write", action)
		}
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatal("admin refusal wrote a rollback record", entries, err)
	}
}

func stopUpgradeAtHelmWrite(deps *Deps) (*int, error) {
	writes := 0
	stop := errors.New("stop at Helm write")
	helm := deps.RunHelm
	deps.RunHelm = func(ctx context.Context, args []string) ([]byte, error) {
		if args[0] == "upgrade" && !slices.Contains(args, "--dry-run=server") {
			writes++
			return nil, stop
		}
		return helm(ctx, args)
	}
	return &writes, stop
}

func upgradePreflightPod(name string, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: DefaultNamespace, Labels: map[string]string{"app.kubernetes.io/instance": "oberth"}},
		Status: corev1.PodStatus{Phase: phase, Conditions: []corev1.PodCondition{
			{Type: corev1.PodReady, Status: corev1.ConditionTrue},
		}},
	}
}

func TestUpgradePreflightPodObservationErrors(t *testing.T) {
	for _, kind := range []string{"multiple Running Pods", "Pod list error"} {
		t.Run(kind, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			for _, name := range []string{"OBERTH_MCP_URL", "OBERTH_TOKEN_COMMAND", "OBERTH_CLIENT_CERT", "OBERTH_CLIENT_KEY", "OBERTH_CA_CERT"} {
				t.Setenv(name, "")
			}
			t.Setenv("OBERTH_BASE_URL", "https://server.example.test:8443")
			t.Setenv("OBERTH_TOKEN", "fixture")
			f := newUpgradeFixture(t, "0.22.7", "0.22.8", "99")
			f.deps.Admin = nil
			f.deps.ClientConfigRoot = func() (string, error) { return t.TempDir(), nil }
			kube := f.deps.KubeClient.(*fake.Clientset)
			cause := errors.New("Pod list refused")
			if kind == "Pod list error" {
				kube.PrependReactor("list", "pods", func(ktesting.Action) (bool, runtime.Object, error) { return true, nil, cause })
			} else {
				for _, name := range []string{"one", "two"} {
					if _, err := kube.CoreV1().Pods(DefaultNamespace).Create(t.Context(), upgradePreflightPod(name, corev1.PodRunning), metav1.CreateOptions{}); err != nil {
						t.Fatal(err)
					}
				}
			}
			kube.ClearActions()
			writes, _ := stopUpgradeAtHelmWrite(&f.deps)
			_, err := RunUpgrade(t.Context(), UpgradeConfig{BinaryVersion: "0.22.8", Namespace: DefaultNamespace, NoBackup: true}, f.deps)
			if err == nil || *writes != 0 || strings.Contains(err.Error(), "configure it with") {
				t.Fatal("Pod observation error escaped plain preflight refusal", *writes, err)
			}
			if kind == "Pod list error" && !errors.Is(err, cause) {
				t.Fatal("Pod list error lost", err)
			}
			if kind == "multiple Running Pods" && !strings.Contains(err.Error(), "requires one running server Pod") {
				t.Fatal("ambiguous running Pods not reported", err)
			}
		})
	}
}

func TestUpgradePreflightClusterMismatchMessage(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	deps := Deps{ContextName: "recorded", RestConfig: &rest.Config{Host: "https://recorded.invalid"}, ClientConfigRoot: client.ConfigDir}
	if err := recordKubernetesTarget(Config{Namespace: DefaultNamespace}, deps); err != nil {
		t.Fatal(err)
	}
	deps.RestConfig.Host = "https://different.invalid"
	for _, injected := range []bool{false, true} {
		if injected {
			deps.Admin = fixtureAdmin{}
		}
		err := checkUpgradeVerificationConnection(t.Context(), UpgradeConfig{}, deps)
		if err == nil || !strings.Contains(err.Error(), "recorded Kubernetes cluster identity changed") || strings.Contains(err.Error(), "configure it with") {
			t.Fatal("cluster mismatch needs its own message", injected, err)
		}
	}
}
