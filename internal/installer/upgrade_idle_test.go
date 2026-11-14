package installer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"k8s.io/client-go/rest"
)

func upgradeWorkflowList(phases ...string) string {
	items := make([]any, 0, len(phases))
	for i, phase := range phases {
		items = append(items, map[string]any{
			"metadata": map[string]any{"name": "workflow-" + string(rune('a'+i)), "namespace": "selected-pipelines", "labels": map[string]string{"oberth.ci/run": "run-" + string(rune('a'+i))}},
			"status":   map[string]string{"phase": phase},
		})
	}
	raw, _ := json.Marshal(map[string]any{"apiVersion": "argoproj.io/v1alpha1", "kind": "WorkflowList", "metadata": map[string]any{}, "items": items})
	return string(raw)
}

func withUpgradeWorkflowAPI(t testing.TB, deps *Deps, handler http.HandlerFunc) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	deps.RestConfig = &rest.Config{Host: server.URL}
}

func TestUpgradeDefaultWaitsBeforeAnyChange(t *testing.T) {
	fixture := newUpgradeFixture(t, "0.22.7", "0.22.8", "73")
	fixture.deps.PollInterval = time.Millisecond
	var probes atomic.Int32
	var idle atomic.Bool
	runHelm := fixture.deps.RunHelm
	fixture.deps.RunHelm = func(ctx context.Context, args []string) ([]byte, error) {
		if args[0] == "get" && args[1] == "values" && strings.Contains(strings.Join(args, " "), "--all") {
			raw, err := runHelm(ctx, args)
			var values map[string]any
			if err == nil {
				err = json.Unmarshal(raw, &values)
			}
			if err != nil {
				return nil, err
			}
			if !strings.Contains(strings.Join(args, " "), "--revision 1") {
				t.Fatalf("namespace read must use the deployed revision: %v", args)
			}
			values["argo"] = map[string]any{"namespace": "selected-pipelines"}
			return json.Marshal(values)
		}
		if args[0] == "upgrade" && !strings.Contains(strings.Join(args, " "), "--dry-run=server") && !idle.Load() {
			t.Fatal("Helm changed before idle")
		}
		return runHelm(ctx, args)
	}
	withUpgradeWorkflowAPI(t, &fixture.deps, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/apis/argoproj.io/v1alpha1/namespaces/selected-pipelines/workflows" || r.URL.Query().Get("labelSelector") != "oberth.ci/run" {
			t.Errorf("unexpected API request: %s %s", r.Method, r.URL)
		}
		w.Header().Set("Content-Type", "application/json")
		if probes.Add(1) < 3 {
			_, _ = io.WriteString(w, upgradeWorkflowList("Running", "Pending", "", "Succeeded", "Failed", "Error"))
		} else {
			idle.Store(true)
			_, _ = io.WriteString(w, upgradeWorkflowList())
		}
	})
	backupCalled := false
	result, err := RunUpgrade(t.Context(), UpgradeConfig{Namespace: "oberth", BinaryVersion: "0.22.8", BinarySchemaVersion: 73,
		Backup: func(context.Context) (UpgradeBackup, error) {
			backupCalled = true
			if !idle.Load() {
				t.Fatal("backup before idle")
			}
			return UpgradeBackup{Path: filepath.Join(t.TempDir(), "backup.enc")}, nil
		}}, fixture.deps)
	text := fixture.output.String()
	if err != nil || !result.Upgraded || !backupCalled || probes.Load() != 3 || !strings.Contains(text, "3 active Workflows; time left 2h0m0s") || !strings.Contains(text, "0 active Workflows") || strings.ContainsAny(text, "\r\033") {
		t.Fatalf("result=%+v probes=%d backup=%t err=%v output=%s", result, probes.Load(), backupCalled, err, text)
	}
}

func TestUpgradeWaitTimeoutRefusesWithoutChanges(t *testing.T) {
	fixture := newUpgradeFixture(t, "0.22.7", "0.22.8", "73")
	fixture.deps.PollInterval = 40 * time.Millisecond
	withUpgradeWorkflowAPI(t, &fixture.deps, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, upgradeWorkflowList("Running", "Pending"))
	})
	dir := t.TempDir()
	result, err := RunUpgrade(t.Context(), UpgradeConfig{Namespace: "oberth", BinaryVersion: "0.22.8", BinarySchemaVersion: 73, WaitTimeout: 100 * time.Millisecond, RecordDir: dir,
		Backup: func(context.Context) (UpgradeBackup, error) {
			t.Fatal("backup before idle")
			return UpgradeBackup{}, nil
		}}, fixture.deps)
	if !errors.Is(err, context.DeadlineExceeded) || result.Upgraded || !strings.Contains(err.Error(), "upgrade not attempted") || !strings.Contains(err.Error(), "run-a") || !strings.Contains(err.Error(), "run-b") || !strings.Contains(err.Error(), "workflow-a") {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	for _, call := range *fixture.calls {
		if strings.HasPrefix(call, "upgrade ") && !strings.Contains(call, "--dry-run=server") {
			t.Fatalf("mutation on timeout: %s", call)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 || result.RecordPath != "" {
		t.Fatalf("record written on timeout: %v %v", entries, err)
	}
}

func TestUpgradeNoWaitIdleAndNegativeControls(t *testing.T) {
	for _, mode := range []string{"skip", "dry-run", "same-version", "same-version-values", "same-version-failed", "downgrade"} {
		t.Run(mode, func(t *testing.T) {
			installed := "0.16.20"
			if strings.HasPrefix(mode, "same-version") {
				installed = "0.16.21"
			}
			if mode == "downgrade" {
				installed = "0.16.22"
			}
			fixture := newUpgradeFixture(t, installed, "0.16.21", "73")
			cfg := UpgradeConfig{Namespace: "oberth", BinaryVersion: "0.16.21", BinarySchemaVersion: 73, NoBackup: true, NoWaitIdle: mode == "skip", DryRun: mode == "dry-run", AllowDowngrade: mode == "downgrade"}
			if mode == "same-version-values" {
				values := filepath.Join(t.TempDir(), "values.yaml")
				if err := os.WriteFile(values, []byte("{}\n"), 0600); err != nil {
					t.Fatal(err)
				}
				cfg.UserValues = []string{values}
			}
			if mode == "same-version-failed" {
				run := fixture.deps.RunHelm
				fixture.deps.RunHelm = func(ctx context.Context, args []string) ([]byte, error) {
					raw, err := run(ctx, args)
					if args[0] == "list" {
						raw = bytes.ReplaceAll(raw, []byte(`"status":"deployed"`), []byte(`"status":"failed"`))
					}
					return raw, err
				}
			}
			var probes atomic.Int32
			withUpgradeWorkflowAPI(t, &fixture.deps, func(w http.ResponseWriter, _ *http.Request) {
				probes.Add(1)
				http.Error(w, "must not observe", http.StatusForbidden)
			})
			if mode == "downgrade" {
				cfg.WaitTimeout = time.Second
				_, err := RunUpgrade(t.Context(), cfg, fixture.deps)
				if err == nil || probes.Load() != 1 {
					t.Fatalf("downgrade bypassed waiting: probes=%d err=%v", probes.Load(), err)
				}
				return
			}
			result, err := RunUpgrade(t.Context(), cfg, fixture.deps)
			if err != nil || probes.Load() != 0 {
				t.Fatalf("result=%+v probes=%d err=%v", result, probes.Load(), err)
			}
			text := fixture.output.String()
			if mode == "skip" && (!result.Upgraded || strings.Count(text, "running Workflows of the previous version will fail on their next step") != 1) {
				t.Fatal(text)
			}
			if mode == "dry-run" && !strings.Contains(text, "The real upgrade will wait") {
				t.Fatal(text)
			}
			if strings.HasPrefix(mode, "same-version") && strings.Contains(text, "Waiting before upgrade") {
				t.Fatal(text)
			}
		})
	}
}

func TestUpgradeWaitFailsClosed(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusNotFound, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			fixture := newUpgradeFixture(t, "0.16.20", "0.16.21", "73")
			withUpgradeWorkflowAPI(t, &fixture.deps, func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "fixture refusal", status) })
			_, err := RunUpgrade(t.Context(), UpgradeConfig{Namespace: "oberth", BinaryVersion: "0.16.21", NoBackup: true, WaitTimeout: time.Second}, fixture.deps)
			if err == nil || !strings.Contains(err.Error(), "upgrade not attempted") {
				t.Fatal(err)
			}
			for _, call := range *fixture.calls {
				if strings.HasPrefix(call, "upgrade ") && !strings.Contains(call, "--dry-run=server") {
					t.Fatal(call)
				}
			}
		})
	}
}

func TestUpgradeIdleTerminalCountdown(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		var output bytes.Buffer
		for _, remaining := range []time.Duration{time.Minute, 45 * time.Second} {
			if err := printUpgradeIdleProgress(&output, terminal, 2, remaining); err != nil {
				t.Fatal(err)
			}
		}
		text := output.String()
		if !strings.Contains(text, "2 active Workflows; time left 1m0s") || !strings.Contains(text, "time left 45s") {
			t.Fatal(text)
		}
		if terminal && (strings.Count(text, "\r\033[2K") != 2 || strings.Contains(text, "\n")) {
			t.Fatalf("terminal=%q", text)
		}
		if !terminal && (strings.Count(text, "\n") != 2 || strings.ContainsAny(text, "\r\033")) {
			t.Fatalf("pipe=%q", text)
		}
	}
}

func TestUpgradeWaitDeadlineRetainsRunsAndCancellation(t *testing.T) {
	for _, mode := range []string{"deadline", "cancel", "late-idle"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				fixture := newUpgradeFixture(t, "0.16.20", "0.16.21", "73")
				fixture.deps.PollInterval = time.Second
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				wantErr := context.DeadlineExceeded
				if mode == "cancel" {
					wantErr = context.Canceled
					time.AfterFunc(1500*time.Millisecond, cancel)
				}
				calls := 0
				fixture.deps.RestConfig = &rest.Config{Host: "http://fixture", Transport: upgradeIdleRoundTrip(func(r *http.Request) (*http.Response, error) {
					calls++
					body := upgradeWorkflowList("Running")
					if calls > 1 {
						<-r.Context().Done()
						if mode != "late-idle" {
							return nil, r.Context().Err()
						}
						body = upgradeWorkflowList()
					}
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
				})}
				err := waitForUpgradeIdle(ctx, UpgradeConfig{Namespace: "oberth", WaitTimeout: 3 * time.Second}, fixture.deps)
				if !errors.Is(err, wantErr) || !strings.Contains(err.Error(), "upgrade not attempted") || (mode != "late-idle" && !strings.Contains(err.Error(), "run-a")) {
					t.Fatalf("calls=%d err=%v", calls, err)
				}
			})
		})
	}
}

func TestUpgradeWaitObservesEveryPage(t *testing.T) {
	fixture := newUpgradeFixture(t, "0.16.20", "0.16.21", "73")
	var probes atomic.Int32
	withUpgradeWorkflowAPI(t, &fixture.deps, func(w http.ResponseWriter, r *http.Request) {
		probes.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("continue") == "" {
			_, _ = io.WriteString(w, `{"apiVersion":"argoproj.io/v1alpha1","kind":"WorkflowList","metadata":{"continue":"next-page"},"items":[]}`)
		} else {
			if r.URL.Query().Get("continue") != "next-page" {
				t.Error(r.URL)
			}
			_, _ = io.WriteString(w, upgradeWorkflowList("Running"))
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	err := waitForUpgradeIdle(ctx, UpgradeConfig{Namespace: "oberth"}, fixture.deps)
	if !errors.Is(err, context.DeadlineExceeded) || probes.Load() != 2 || !strings.Contains(err.Error(), "run-a") {
		t.Fatalf("probes=%d err=%v", probes.Load(), err)
	}
}
