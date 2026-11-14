//go:build linux

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

// Run the actual CLI with a private kubeconfig and fixture host commands.
func TestUpgradeDefaultWaitProcessHelper(t *testing.T) {
	var extra []string
	switch os.Getenv("OBERTH_TEST_WAIT_ROLE") {
	case "cli-default", "cli-already-current", "cli-unavailable":
	case "cli-timeout":
		extra = []string{"--wait-for=200ms"}
	case "cli-skip":
		extra = []string{"--no-wait-idle"}
	case "cli-explicit":
		extra = []string{"--wait-idle"}
	case "cli-dry-run":
		extra = []string{"--dry-run"}
	case "cli-rollback":
		extra = []string{"--rollback=missing-record.json"}
	default:
		return
	}
	version = "0.17.52"
	home, err := os.UserHomeDir()
	if err != nil {
		os.Exit(93)
	}
	os.Args = append([]string{"oberth", "upgrade", "--chart=/fixture", "--timeout=1m", "--wait-for=30s", "--no-backup", "--backup-dir=" + home}, extra...)
	main()
	os.Exit(0)
}

func TestUpgradeDefaultWaitProcessGraph(t *testing.T) {
	for _, mode := range []string{"default", "explicit", "timeout", "skip", "dry-run", "already-current", "rollback", "unavailable"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(root, "upgrade.attempted")
			var probes atomic.Int32
			kubeconfig, adminURL := upgradeDefaultWaitKubeconfig(t, root, func(w http.ResponseWriter, r *http.Request) {
				probe := probes.Add(1)
				if r.Method != http.MethodGet || r.URL.Query().Get("labelSelector") != "oberth.ci/run" {
					t.Errorf("unexpected workflow request %s %s", r.Method, r.URL)
				}
				if mode == "unavailable" {
					http.Error(w, "fixture unavailable", http.StatusForbidden)
					return
				}
				items := `[]`
				if mode != "default" && mode != "explicit" || probe < 3 {
					items = `[{"metadata":{"namespace":"oberth-argo","name":"workflow-busy","labels":{"oberth.ci/run":"run-busy"}},"status":{"phase":"Running"}}]`
				}
				fmt.Fprintf(w, `{"apiVersion":"argoproj.io/v1alpha1","kind":"WorkflowList","items":%s}`, items)
			})
			helm := `#!/bin/sh
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
			kubectl := `#!/bin/sh
case "$*" in
 *"version --schema"*) echo 31 ;;
 *) echo 'unexpected fixture kubectl command' >&2; exit 91 ;;
esac
`
			for name, body := range map[string]string{"helm": helm, "kubectl": kubectl} {
				if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0700); err != nil {
					t.Fatal(err)
				}
			}
			already := "no"
			if mode == "already-current" {
				already = "yes"
			}
			command := exec.CommandContext(t.Context(), executable, "-test.run=^TestUpgradeDefaultWaitProcessHelper$")
			command.Dir = root
			command.Env = []string{"OBERTH_BASE_URL=" + adminURL, "OBERTH_TOKEN=fixture-admin", "PATH=" + root + ":/usr/bin:/bin", "HOME=" + root, "KUBECONFIG=" + kubeconfig, "OBERTH_TEST_WAIT_ROLE=cli-" + mode, "OBERTH_TEST_UPGRADE_MARKER=" + marker, "OBERTH_TEST_ALREADY=" + already}
			raw, err := command.CombinedOutput()
			output := string(raw)
			_, statErr := os.Stat(marker)
			changed := statErr == nil
			switch mode {
			case "default", "explicit", "skip":
				if !changed || err == nil || !strings.Contains(output, "fixture Helm upgrade refusal") {
					t.Fatalf("changed=%t err=%v output=%s", changed, err, output)
				}
				if mode == "skip" {
					if probes.Load() != 0 || strings.Count(output, "running Workflows of the previous version will fail on their next step") != 1 {
						t.Fatal(output)
					}
				} else if probes.Load() != 3 || !strings.Contains(output, "1 active Workflows; time left") || !strings.Contains(output, "0 active Workflows") || strings.ContainsAny(output, "\r\033") {
					t.Fatalf("probes=%d output=%s", probes.Load(), output)
				}
			case "dry-run", "already-current":
				if err != nil || changed || probes.Load() != 0 {
					t.Fatalf("changed=%t probes=%d err=%v output=%s", changed, probes.Load(), err, output)
				}
				if mode == "dry-run" && !strings.Contains(output, "The real upgrade will wait") {
					t.Fatal(output)
				}
			default:
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != 1 || changed {
					t.Fatalf("changed=%t err=%v output=%s", changed, err, output)
				}
				if mode == "timeout" && (!strings.Contains(output, "run-busy") || !strings.Contains(output, "upgrade not attempted")) {
					t.Fatal(output)
				}
				if mode == "rollback" && probes.Load() != 0 {
					t.Fatal(output)
				}
				if mode == "unavailable" && !strings.Contains(output, "upgrade not attempted") {
					t.Fatal(output)
				}
			}
		})
	}
}

func upgradeDefaultWaitKubeconfig(t *testing.T, root string, workflows http.HandlerFunc) (string, string) {
	t.Helper()
	objects := upgradeWaitArgoObjects()
	deployment := objects[0].(*appsv1.Deployment)
	config := objects[1].(*corev1.ConfigMap)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/admin/version" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			t.Errorf("Kubernetes mutation: %s %s", r.Method, r.URL.Path)
			http.Error(w, "refused", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/apis/argoproj.io/v1alpha1/namespaces/oberth-argo/workflows":
			workflows(w, r)
		case "/apis/apps/v1/namespaces/" + deployment.Namespace + "/deployments":
			_ = json.NewEncoder(w).Encode(&appsv1.DeploymentList{Items: []appsv1.Deployment{*deployment}})
		case "/api/v1/namespaces/" + config.Namespace + "/configmaps/" + config.Name:
			_ = json.NewEncoder(w).Encode(config)
		case "/api/v1/namespaces/kube-system":
			http.NotFound(w, r)
		default:
			t.Errorf("unexpected Kubernetes request: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	kubeconfig := filepath.Join(root, "kubeconfig")
	if err := os.WriteFile(kubeconfig, []byte("apiVersion: v1\nkind: Config\ncurrent-context: fixture\ncontexts:\n- name: fixture\n  context:\n    cluster: fixture\n    user: fixture\nclusters:\n- name: fixture\n  cluster:\n    server: "+server.URL+"\nusers:\n- name: fixture\n  user: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return kubeconfig, server.URL
}
