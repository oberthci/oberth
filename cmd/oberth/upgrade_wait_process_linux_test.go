//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/testtimeout"
)

func upgradeWaitArgoObjects() []runtime.Object {
	return []runtime.Object{
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "workflow-controller", Namespace: installer.DefaultArgoNamespace}, Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "workflow-controller", Command: []string{"workflow-controller"}, Args: []string{"--configmap", "idle-test-config"}}}}}}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "idle-test-config", Namespace: installer.DefaultArgoNamespace}, Data: map[string]string{"config": `{
			"mainContainer":{"securityContext":{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]},"seccompProfile":{"type":"RuntimeDefault"}}},
			"executor":{"securityContext":{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]},"seccompProfile":{"type":"RuntimeDefault"}}}
		}`}},
	}
}

// The helper runs the actual CLI; all cluster traffic is loopback and every
// kubectl invocation refuses. Idle observation creates no child observer.
func TestUpgradeWaitProcessHelper(t *testing.T) {
	if os.Getenv("OBERTH_TEST_WAIT_ROLE") != "cli" {
		return
	}
	version = "0.17.52"
	home, err := os.UserHomeDir()
	if err != nil {
		os.Exit(93)
	}
	os.Args = []string{"oberth", "upgrade", "--context=fixture", "--wait-idle", "--chart=/fixture", "--timeout=1m", "--wait-timeout=" + os.Getenv("OBERTH_TEST_WAIT_LIMIT"), "--no-backup", "--backup-dir=" + home}
	if os.Getenv("OBERTH_TEST_OLD_CRITICAL") == "yes" {
		os.Args = append(os.Args, "--wait-for=critical")
	}
	main()
	os.Exit(0)
}

type upgradeWaitProcessFixture struct {
	root, kubeconfig, mode string
	firstProbe             atomic.Int64
	liveReads              atomic.Int32
	observed               chan struct{}
	busyFor                time.Duration
}

func newUpgradeWaitProcessFixture(t *testing.T, mode string) *upgradeWaitProcessFixture {
	t.Helper()
	f := &upgradeWaitProcessFixture{root: t.TempDir(), mode: mode, observed: make(chan struct{}), busyFor: 100 * time.Millisecond}
	if os.Getenv("OBERTH_TEST_LONG_IDLE_WAIT") == "1" {
		f.busyFor = 121 * time.Second
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { f.serve(t, w, r) }))
	t.Cleanup(server.Close)
	f.kubeconfig = filepath.Join(f.root, "kubeconfig")
	config := "apiVersion: v1\nkind: Config\ncurrent-context: fixture\ncontexts:\n- name: fixture\n  context:\n    cluster: fixture\n    user: fixture\nclusters:\n- name: fixture\n  cluster:\n    server: " + server.URL + "\nusers:\n- name: fixture\n  user: {}\n"
	if err := os.WriteFile(f.kubeconfig, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"helm": upgradeWaitHelmFixture, "kubectl": "#!/bin/sh\nprintf refused > \"$(dirname \"$0\")/unexpected-kubectl\"\nexit 91\n"} {
		if err := os.WriteFile(filepath.Join(f.root, name), []byte(body), 0700); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func (f *upgradeWaitProcessFixture) serve(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	if r.Method != http.MethodGet {
		t.Errorf("upgrade mutated Kubernetes: %s %s", r.Method, r.URL.Path)
		http.Error(w, "refused", 405)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	objects := upgradeWaitArgoObjects()
	deployment := objects[0].(*appsv1.Deployment)
	config := objects[1].(*corev1.ConfigMap)
	switch r.URL.Path {
	case "/apis/apps/v1/namespaces/" + deployment.Namespace + "/deployments":
		f.liveReads.Add(1)
		_ = json.NewEncoder(w).Encode(&appsv1.DeploymentList{Items: []appsv1.Deployment{*deployment}})
	case "/api/v1/namespaces/" + config.Namespace + "/configmaps/" + config.Name:
		f.liveReads.Add(1)
		_ = json.NewEncoder(w).Encode(config)
	case "/api/v1/namespaces/kube-system":
		http.NotFound(w, r)
	case "/apis/argoproj.io/v1alpha1/namespaces/oberth-argo/workflows":
		f.workflows(t, w, r)
	default:
		t.Errorf("unexpected Kubernetes request: %s", r.URL.Path)
		http.NotFound(w, r)
	}
}

func (f *upgradeWaitProcessFixture) workflows(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	if r.URL.Query().Get("labelSelector") != "oberth.ci/run" {
		t.Error("idle observation lost run selector")
	}
	if f.liveReads.Load() < 2 {
		t.Error("idle observation preceded live Argo configuration check")
	}
	if f.firstProbe.CompareAndSwap(0, time.Now().UnixNano()) {
		close(f.observed)
	}
	if f.mode == "unavailable" {
		http.Error(w, "fixture unavailable", 503)
		return
	}
	if f.mode == "observer-disconnected" {
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Error("missing HTTP hijacker")
			return
		}
		conn, _, err := hijacker.Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_ = conn.Close()
		return
	}
	phase := "Running"
	if (f.mode == "shell" || f.mode == "setsid" || f.mode == "old-critical") && time.Since(time.Unix(0, f.firstProbe.Load())) >= f.busyFor {
		phase = "Succeeded"
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": "argoproj.io/v1alpha1", "kind": "WorkflowList", "items": []any{map[string]any{"metadata": map[string]any{"name": "fixture", "labels": map[string]string{"oberth.ci/run": "run"}}, "status": map[string]any{"phase": phase}}}})
}

func (f *upgradeWaitProcessFixture) command(t *testing.T) *exec.Cmd {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), testtimeout.Start(170*time.Second))
	t.Cleanup(cancel)
	command := exec.CommandContext(ctx, "/bin/sh", "-c", `exec "$@"`, "fixture", executable, "-test.run", "^TestUpgradeWaitProcessHelper$")
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if f.mode == "setsid" {
		setsid, err := exec.LookPath("setsid")
		if err != nil {
			t.Fatal(err)
		}
		command = exec.CommandContext(ctx, setsid, "/bin/sh", "-c", `exec "$@"`, "fixture", executable, "-test.run", "^TestUpgradeWaitProcessHelper$")
	}
	limit, already, critical := "3m", "no", "no"
	if f.mode == "deadline" {
		limit = "250ms"
	}
	if f.mode == "already-current" {
		already = "yes"
	}
	if f.mode == "old-critical" {
		critical = "yes"
	}
	command.Env = []string{"PATH=" + f.root + ":/usr/bin:/bin", "HOME=" + f.root, "KUBECONFIG=" + f.kubeconfig, "OBERTH_TEST_WAIT_ROLE=cli", "OBERTH_TEST_WAIT_LIMIT=" + limit, "OBERTH_TEST_UPGRADE_MARKER=" + filepath.Join(f.root, "upgrade.attempted"), "OBERTH_TEST_ALREADY=" + already, "OBERTH_TEST_OLD_CRITICAL=" + critical}
	if scale, set := os.LookupEnv("OBERTH_TEST_TIMEOUT_SCALE"); set {
		command.Env = append(command.Env, "OBERTH_TEST_TIMEOUT_SCALE="+scale)
	}
	command.WaitDelay = 5 * time.Second
	return command
}

func TestUpgradeWaitProcessGraph(t *testing.T) {
	for _, mode := range []string{"shell", "setsid", "observer-disconnected", "kill-group", "cancel", "deadline", "unavailable", "already-current", "old-critical"} {
		t.Run(mode, func(t *testing.T) { t.Parallel(); f := newUpgradeWaitProcessFixture(t, mode); f.run(t) })
	}
}

func (f *upgradeWaitProcessFixture) run(t *testing.T) {
	t.Helper()
	command := f.command(t)
	var output strings.Builder
	command.Stdout, command.Stderr = &output, &output
	start := time.Now()
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL) })
	finished := make(chan error, 1)
	go func() { finished <- command.Wait() }()
	if f.mode == "cancel" || f.mode == "kill-group" {
		timer := time.NewTimer(testtimeout.Start(15 * time.Second))
		defer timer.Stop()
		select {
		case err := <-finished:
			t.Fatalf("CLI exited before native observation: %v: %s", err, &output)
		case <-timer.C:
			t.Fatal("native observer did not start")
		case <-f.observed:
		}
		if f.mode == "kill-group" {
			if err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL); err != nil {
				t.Fatal(err)
			}
		} else if err := command.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
	}
	err := <-finished
	f.check(t, err, output.String(), time.Since(start))
}

func (f *upgradeWaitProcessFixture) check(t *testing.T, err error, text string, elapsed time.Duration) {
	t.Helper()
	if _, e := os.Stat(filepath.Join(f.root, "unexpected-kubectl")); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("upgrade invoked kubectl instead of the native observer")
	}
	_, markerErr := os.Stat(filepath.Join(f.root, "upgrade.attempted"))
	attempted := markerErr == nil
	if f.mode == "already-current" {
		if err != nil || attempted || f.firstProbe.Load() != 0 || !strings.Contains(text, "Already running v0.17.52; no upgrade performed") {
			t.Fatalf("same-version no-op: %v: %s", err, text)
		}
		return
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("expected nonzero CLI exit: %v: %s", err, text)
	}
	switch f.mode {
	case "shell", "setsid", "old-critical":
		if !attempted || elapsed < f.busyFor || exit.ExitCode() != 1 || !strings.Contains(text, "fixture Helm upgrade refusal") {
			t.Fatalf("elapsed=%s attempted=%v err=%v: %s", elapsed, attempted, err, text)
		}
	default:
		if attempted {
			t.Fatal("Helm mutation followed refused native observation")
		}
		switch f.mode {
		case "kill-group":
			if !exit.Sys().(syscall.WaitStatus).Signaled() || exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL || strings.Contains(text, "oberth: wait for") {
				t.Fatalf("group kill: %v: %s", err, text)
			}
		case "cancel":
			if exit.ExitCode() != 130 || strings.Contains(text, "context canceled") || strings.Contains(text, "oberth:") {
				t.Fatalf("cancel: %v: %s", err, text)
			}
		case "deadline":
			if exit.ExitCode() != 1 || !strings.Contains(text, "context deadline exceeded") {
				t.Fatalf("deadline: %v: %s", err, text)
			}
		case "unavailable", "observer-disconnected":
			if exit.ExitCode() != 1 || !strings.Contains(text, "cannot observe active Kubernetes Workflows") {
				t.Fatalf("failed observer: %v: %s", err, text)
			}
		}
	}
	t.Logf("mode=%s elapsed=%s upgrade_attempted=%v", f.mode, elapsed, attempted)
}

const upgradeWaitHelmFixture = `#!/bin/sh
case "$*" in
 *"version --template "*) printf 'v4.2.3\n' ;;
 *"history oberth "*"-o json"*) printf '[{"revision":1,"status":"deployed"}]\n' ;;
 *"get values oberth "*"--all"*) printf '{"compatibility":{"schemaIdentity":"legacy-schema-v1","auditDomain":"legacy-audit-v1","witnessKeyInfo":"legacy-witness-v1"}}\n' ;;
 *"get values oberth "*) printf '{"argo":{"podPolicy":{"enabled":false}}}\n' ;;
 *"template oberth "*) exit 0 ;;
 *"list "*)
   if [ "$OBERTH_TEST_ALREADY" = yes ]; then chart=oberth-0.17.52; else chart=oberth-0.17.51; fi
   printf '[{"name":"oberth","revision":1,"chart":"%s","status":"deployed"}]\n' "$chart" ;;
 *"show values "*) printf 'image:\n  ref: example.invalid/oberth@sha256:1111111111111111111111111111111111111111111111111111111111111111\n' ;;
 *"upgrade oberth "*"--dry-run=server"*) printf '{"config":{"argo":{"podPolicy":{"enabled":false}}}}\n' ;;
 *"upgrade oberth "*) : > "$OBERTH_TEST_UPGRADE_MARKER"; echo 'fixture Helm upgrade refusal' >&2; exit 73 ;;
 *) echo 'unexpected fixture Helm command' >&2; exit 91 ;;
esac
`
