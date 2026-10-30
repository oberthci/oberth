//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/testtimeout"
)

// The helper runs the actual CLI main, or the production in-pod wait loop in
// place of kubectl exec. Every host command is a fixture; no cluster is used.
func TestUpgradeWaitProcessHelper(t *testing.T) {
	switch os.Getenv("OBERTH_TEST_WAIT_ROLE") {
	case "cli":
		version = "0.17.52"
		os.Args = []string{"oberth", "upgrade", "--yes", "--wait-idle", "--chart=/fixture", "--timeout=1m", "--wait-timeout=" + os.Getenv("OBERTH_TEST_WAIT_LIMIT")}
		if os.Getenv("OBERTH_TEST_OLD_CRITICAL") == "yes" {
			os.Args = append(os.Args, "--wait-for=critical")
		}
		main()
		os.Exit(0)
	case "waiter":
		if err := os.WriteFile(os.Getenv("OBERTH_TEST_WAIT_PID"), []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
			os.Exit(92)
		}
		pgid, _ := syscall.Getpgid(0)
		fmt.Printf("fixture observer pid=%d ppid=%d pgid=%d\n", os.Getpid(), os.Getppid(), pgid)
		ctx, cancel := context.WithTimeout(context.Background(), testtimeout.Start(3*time.Minute))
		defer cancel()
		err := waitIdle(ctx, &http.Client{Timeout: 5 * time.Second}, os.Getenv("OBERTH_TEST_WAIT_URL"), time.Second, idleWaitOptions{mode: "all", output: os.Stdout})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
}

func TestUpgradeWaitProcessGraph(t *testing.T) {
	// Default coverage exercises the same process graph with a short busy wait.
	// Opt in to the original >2 minute reproduction when investigating callers:
	// OBERTH_TEST_LONG_IDLE_WAIT=1 go test -run '^TestUpgradeWaitProcessGraph$' ./cmd/oberth
	busyFor := 100 * time.Millisecond
	if os.Getenv("OBERTH_TEST_LONG_IDLE_WAIT") == "1" {
		busyFor = 121 * time.Second
	}
	for _, mode := range []string{"shell", "setsid", "kill-child", "kill-group", "cancel", "deadline", "unavailable", "already-current", "old-critical"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			pidPath, upgradePath := filepath.Join(root, "wait.pid"), filepath.Join(root, "upgrade.attempted")
			var firstProbe atomic.Int64
			admin := newAdminGateServer(nil, "fixture", nil, func(context.Context) (model.IdleQueueStatus, error) {
				if mode == "unavailable" {
					return model.IdleQueueStatus{}, errors.New("fixture unavailable")
				}
				firstProbe.CompareAndSwap(0, time.Now().UnixNano())
				if (mode == "shell" || mode == "setsid") && time.Since(time.Unix(0, firstProbe.Load())) >= busyFor {
					return model.IdleQueueStatus{Version: 1}, nil
				}
				return model.IdleQueueStatus{Version: 1, Queued: 1, Branch: 1}, nil
			})
			server := httptest.NewServer(admin.Handler)
			t.Cleanup(server.Close)
			helm := `#!/bin/sh
case "$*" in
 *"version --template "*) printf 'v4.2.3\n' ;;
 *"history oberth "*"-o json"*) printf '[{"revision":1,"status":"deployed"}]\n' ;;
 *"get values oberth "*"--all"*) printf '{"compatibility":{"schemaIdentity":"legacy-schema-v1","auditDomain":"legacy-audit-v1","witnessKeyInfo":"legacy-witness-v1"}}\n' ;;
 *"get values oberth "*) printf '{}\n' ;;
 *"template oberth "*) exit 0 ;;
 *"list "*)
   if [ "$OBERTH_TEST_ALREADY" = yes ]; then chart=oberth-0.17.52; else chart=oberth-0.17.51; fi
   printf '[{"name":"oberth","revision":1,"chart":"%s","status":"deployed"}]\n' "$chart" ;;
 *"show values "*) printf 'image:\n  ref: example.invalid/oberth@sha256:1111111111111111111111111111111111111111111111111111111111111111\n' ;;
 *"upgrade oberth "*) : > "$OBERTH_TEST_UPGRADE_MARKER"; echo 'fixture Helm upgrade refusal' >&2; exit 73 ;;
 *) echo 'unexpected fixture Helm command' >&2; exit 91 ;;
esac
`
			kubectl := `#!/bin/sh
case "$*" in
 *"wait-idle"*)
   if [ "$OBERTH_TEST_OLD_CRITICAL" = yes ]; then echo 'flag provided but not defined: -wait-for' >&2; exit 2; fi
   OBERTH_TEST_WAIT_ROLE=waiter; export OBERTH_TEST_WAIT_ROLE; exec "$OBERTH_TEST_EXECUTABLE" -test.run '^TestUpgradeWaitProcessHelper$' ;;
 *"version --schema"*) echo 31 ;;
 *) echo 'unexpected fixture kubectl command' >&2; exit 91 ;;
esac
`
			for name, body := range map[string]string{"helm": helm, "kubectl": kubectl} {
				if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0700); err != nil {
					t.Fatal(err)
				}
			}
			kubeconfig := filepath.Join(root, "kubeconfig")
			if err := os.WriteFile(kubeconfig, []byte("apiVersion: v1\nkind: Config\ncurrent-context: fixture\ncontexts:\n- name: fixture\n  context:\n    cluster: fixture\n    user: fixture\nclusters:\n- name: fixture\n  cluster:\n    server: https://127.0.0.1:1\nusers:\n- name: fixture\n  user: {}\n"), 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), testtimeout.Start(170*time.Second))
			defer cancel()
			command := exec.CommandContext(ctx, "/bin/sh", "-c", `exec "$@"`, "fixture", executable, "-test.run", "^TestUpgradeWaitProcessHelper$")
			command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			if mode == "setsid" {
				setsid, err := exec.LookPath("setsid")
				if err != nil {
					t.Fatal("CI process test requires setsid:", err)
				}
				command = exec.CommandContext(ctx, setsid, "/bin/sh", "-c", `exec "$@"`, "fixture", executable, "-test.run", "^TestUpgradeWaitProcessHelper$")
			}
			limit := "3m"
			if mode == "deadline" || mode == "unavailable" {
				limit = "250ms"
			}
			already := "no"
			if mode == "already-current" {
				already = "yes"
			}
			oldCritical := "no"
			if mode == "old-critical" {
				oldCritical = "yes"
			}
			command.Env = []string{"PATH=" + root + ":/usr/bin:/bin", "HOME=" + root, "KUBECONFIG=" + kubeconfig, "OBERTH_TEST_WAIT_ROLE=cli", "OBERTH_TEST_EXECUTABLE=" + executable, "OBERTH_TEST_WAIT_URL=" + server.URL + "/v1/idle", "OBERTH_TEST_WAIT_PID=" + pidPath, "OBERTH_TEST_WAIT_LIMIT=" + limit, "OBERTH_TEST_UPGRADE_MARKER=" + upgradePath, "OBERTH_TEST_ALREADY=" + already, "OBERTH_TEST_OLD_CRITICAL=" + oldCritical}
			// Keep the test-only scale through this private subprocess environment.
			if scale, set := os.LookupEnv("OBERTH_TEST_TIMEOUT_SCALE"); set {
				command.Env = append(command.Env, "OBERTH_TEST_TIMEOUT_SCALE="+scale)
			}
			command.WaitDelay = 5 * time.Second
			var output strings.Builder
			command.Stdout, command.Stderr = &output, &output
			start := time.Now()
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL) })
			finished := make(chan error, 1)
			go func() { finished <- command.Wait() }()
			if mode == "kill-child" || mode == "kill-group" || mode == "cancel" {
				var pid int
				until := time.NewTimer(testtimeout.Start(15 * time.Second))
				defer until.Stop()
				for pid == 0 {
					select {
					case err := <-finished:
						t.Fatalf("CLI exited before observer: %v: %s", err, &output)
					case <-until.C:
						t.Fatal("observer did not start")
					case <-time.After(10 * time.Millisecond):
						if body, err := os.ReadFile(pidPath); err == nil {
							pid, _ = strconv.Atoi(string(body))
						}
					}
				}
				if mode == "kill-child" || mode == "kill-group" {
					// The opt-in long variant also rules out a hidden 60-second kill.
					timer := time.NewTimer(busyFor)
					select {
					case err := <-finished:
						timer.Stop()
						t.Fatalf("wait exited before external kill: %v: %s", err, &output)
					case <-timer.C:
					}
				}
				switch mode {
				case "kill-child":
					if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
						t.Fatal(err)
					}
				case "kill-group":
					if err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL); err != nil {
						t.Fatal(err)
					}
				case "cancel":
					if err := command.Process.Signal(syscall.SIGTERM); err != nil {
						t.Fatal(err)
					}
				}
			}
			err = <-finished
			text := output.String()
			var exit *exec.ExitError
			if mode == "already-current" {
				if err != nil || !strings.Contains(text, "Already running v0.17.52; no upgrade performed") {
					t.Fatalf("same-version no-op: %v: %s", err, text)
				}
				if _, statErr := os.Stat(upgradePath); !errors.Is(statErr, os.ErrNotExist) || firstProbe.Load() != 0 {
					t.Fatalf("same-version no-op attempted wait/upgrade: marker=%v probe=%d", statErr, firstProbe.Load())
				}
				return
			}
			if !errors.As(err, &exit) {
				t.Fatalf("expected nonzero CLI exit, got %v: %s", err, text)
			}
			_, markerErr := os.Stat(upgradePath)
			attempted := markerErr == nil
			if mode == "shell" || mode == "setsid" {
				if !attempted || time.Since(start) < busyFor || exit.ExitCode() != 1 || !strings.Contains(text, "fixture Helm upgrade refusal") {
					t.Fatalf("elapsed=%s attempted=%v exit=%v: %s", time.Since(start), attempted, err, text)
				}
			} else {
				if attempted {
					t.Fatal("upgrade attempted after refused wait")
				}
				switch mode {
				case "kill-group":
					if !exit.Sys().(syscall.WaitStatus).Signaled() || exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL || strings.Contains(text, "oberth: wait for") {
						t.Fatalf("group kill: %v: %s", err, text)
					}
				case "kill-child":
					if exit.ExitCode() != 1 || !strings.Contains(text, "signal: killed") || !strings.Contains(text, "local kubectl exec") {
						t.Fatalf("child kill: %v: %s", err, text)
					}
				case "cancel":
					if exit.ExitCode() != 1 || !strings.Contains(text, "context canceled") {
						t.Fatalf("cancel: %v: %s", err, text)
					}
				case "deadline":
					if exit.ExitCode() != 1 || !strings.Contains(text, "context deadline exceeded") {
						t.Fatalf("deadline: %v: %s", err, text)
					}
				case "unavailable":
					if exit.ExitCode() != 1 || !strings.Contains(text, "HTTP 503") {
						t.Fatalf("unavailable: %v: %s", err, text)
					}
				case "old-critical":
					if exit.ExitCode() != 1 || !strings.Contains(text, "server too old for --wait-for=critical; use --wait-for=all") {
						t.Fatalf("old critical waiter: %v: %s", err, text)
					}
				}
			}
			t.Logf("mode=%s elapsed=%s CLI=%v upgrade_attempted=%v\n%s", mode, time.Since(start), err, attempted, text)
		})
	}
}
