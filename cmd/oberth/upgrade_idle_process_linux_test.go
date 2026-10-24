//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

const upgradeIdleTestImage = "example.invalid/oberth@sha256:" +
	"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// Exercise main's actual exit code and DefaultRunCommand. The PATH tools and
// kubeconfig are fixtures: this never executes host helm/kubectl or a real upgrade.
func TestUpgradeWaitIdleBusyBeyondTwoMinutes(t *testing.T) {
	var first sync.Once
	var busyUntil time.Time
	var calls atomic.Int32
	var idle atomic.Bool
	admin := newAdminGateServer(nil, "unused", nil, func(context.Context) (model.RunCounts, error) {
		first.Do(func() { busyUntil = time.Now().Add(121 * time.Second) })
		calls.Add(1)
		if time.Now().Before(busyUntil) {
			return model.RunCounts{Running: 1}, nil
		}
		idle.Store(true)
		return model.RunCounts{}, nil
	})
	server := httptest.NewServer(admin.Handler)
	defer server.Close()
	cmd, marker := upgradeIdleProcess(t, server.URL, "busy", "4m")
	started := time.Now()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("long busy queue: %v\n%s", err, out)
	}
	if time.Since(started) <= 2*time.Minute || calls.Load() < 2 || !idle.Load() {
		t.Fatalf("did not observe a busy queue for >2 minutes: %s, probes=%d, idle=%t", time.Since(started), calls.Load(), idle.Load())
	}
	if _, err := os.Stat(marker); err != nil || !strings.Contains(string(out), "Ready") {
		t.Fatalf("idle queue did not reach verified upgrade: marker=%v\n%s", err, out)
	}
}

func TestUpgradeWaitIdleFailureExitsNonzeroBeforeMutation(t *testing.T) {
	for _, mode := range []string{"killed", "deadline", "unavailable"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if mode == "unavailable" {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				w.WriteHeader(http.StatusConflict)
			}))
			defer server.Close()
			timeout := "1m"
			if mode == "deadline" {
				timeout = "100ms"
			}
			cmd, marker := upgradeIdleProcess(t, server.URL, mode, timeout)
			out, err := cmd.CombinedOutput()
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
				t.Fatalf("wait failure exit=%v, want 1\n%s", err, out)
			}
			for _, want := range []string{"local kubectl exec failed", "upgrade not attempted"} {
				if !strings.Contains(string(out), want) {
					t.Fatalf("missing %q in error:\n%s", want, out)
				}
			}
			if mode == "killed" && !strings.Contains(string(out), "signal: killed") {
				t.Fatalf("lost local child signal: %s", out)
			}
			if mode == "killed" && strings.Contains(string(out), "context deadline exceeded") {
				t.Fatalf("misreported external child kill as a wait deadline: %s", out)
			}
			if mode == "deadline" && !strings.Contains(string(out), "context deadline exceeded") {
				t.Fatalf("lost wait deadline: %s", out)
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed observation reached helm upgrade: %v", err)
			}
		})
	}
}

func upgradeIdleProcess(t *testing.T, idleURL, mode, timeout string) (*exec.Cmd, string) {
	t.Helper()
	dir := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"helm", "kubectl"} {
		script := "#!/bin/sh\nexec \"$OBERTH_IDLE_TEST_EXE\" -test.run=^TestUpgradeWaitIdleProcessHelper$ -- " + tool + " \"$@\"\n"
		if err := os.WriteFile(filepath.Join(dir, tool), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	marker := filepath.Join(dir, "upgraded")
	kube := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/apis/apps/v1/namespaces/fixture/deployments" {
			t.Errorf("unexpected Kubernetes request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"kind":"DeploymentList","apiVersion":"apps/v1","items":[{"metadata":{"name":"oberth","generation":1},"spec":{"replicas":1,"template":{"spec":{"containers":[{"name":"oberth","image":%q}]}}},"status":{"observedGeneration":1,"updatedReplicas":1,"availableReplicas":1}}]}`, upgradeIdleTestImage)
	}))
	t.Cleanup(kube.Close)
	kubeconfig := filepath.Join(dir, "kubeconfig")
	config := fmt.Sprintf("apiVersion: v1\nkind: Config\ncurrent-context: fixture\ncontexts:\n- name: fixture\n  context: {cluster: fixture, user: fixture}\nclusters:\n- name: fixture\n  cluster: {server: %q}\nusers:\n- name: fixture\n  user: {}\n", kube.URL)
	if err := os.WriteFile(kubeconfig, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestUpgradeWaitIdleProcessHelper$", "--", "upgrade",
		"--yes", "--wait-idle", "--namespace=fixture", "--chart=fixture", "--timeout="+timeout)
	cmd.Env = append(os.Environ(), "OBERTH_IDLE_TEST_EXE="+exe, "OBERTH_IDLE_TEST_URL="+idleURL,
		"OBERTH_IDLE_TEST_MODE="+mode, "OBERTH_IDLE_TEST_MARKER="+marker,
		"KUBECONFIG="+kubeconfig, "PATH="+dir+":"+os.Getenv("PATH"))
	return cmd, marker
}

func TestUpgradeWaitIdleProcessHelper(t *testing.T) {
	if os.Getenv("OBERTH_IDLE_TEST_EXE") == "" {
		return
	}
	separator := slices.Index(os.Args, "--")
	if separator < 0 || separator+1 >= len(os.Args) {
		t.Fatal("missing helper arguments")
	}
	args := os.Args[separator+1:]
	switch args[0] {
	case "upgrade":
		version = "v0.17.52"
		os.Args = append([]string{"oberth"}, args...)
		main()
	case "helm":
		helmArgs := args[1:]
		for len(helmArgs) >= 2 && strings.HasPrefix(helmArgs[0], "--") {
			helmArgs = helmArgs[2:]
		}
		switch helmArgs[0] {
		case "list":
			fmt.Print(`[{"name":"oberth","namespace":"fixture","status":"deployed","chart":"oberth-0.17.51"}]`)
		case "show":
			fmt.Printf("image:\n  ref: %s\n", upgradeIdleTestImage)
		case "upgrade":
			if err := os.WriteFile(os.Getenv("OBERTH_IDLE_TEST_MARKER"), []byte("upgraded"), 0o600); err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatalf("unexpected helm arguments: %v", args)
		}
	case "kubectl":
		switch {
		case slices.Contains(args, "wait-idle"):
			if os.Getenv("OBERTH_IDLE_TEST_MODE") == "killed" {
				process, err := os.FindProcess(os.Getpid())
				if err != nil {
					t.Fatal(err)
				}
				if err := process.Kill(); err != nil {
					t.Fatal(err)
				}
				select {}
			}
			address := strings.TrimPrefix(os.Getenv("OBERTH_IDLE_TEST_URL"), "http://")
			host, _, err := net.SplitHostPort(address)
			if err != nil || host != "127.0.0.1" {
				t.Fatal("idle fixture must use a loopback listener")
			}
			transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "tcp", address)
			}}
			defer transport.CloseIdleConnections()
			client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
			if err := waitIdle(t.Context(), client, "http://admin/v1/idle", time.Second); err != nil {
				t.Fatal(err)
			}
		case slices.Contains(args, "--schema"):
			fmt.Print("31")
		case slices.Contains(args, "version"):
			fmt.Print("oberth v0.17.52 commit=fixture")
		case slices.Contains(args, "audit"):
			_ = json.NewEncoder(os.Stdout).Encode(map[string]bool{"ok": true})
		default:
			t.Fatalf("unexpected kubectl arguments: %v", args)
		}
	default:
		t.Fatalf("unexpected helper: %v", args)
	}
	os.Exit(0)
}
