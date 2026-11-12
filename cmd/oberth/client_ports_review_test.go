package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/oberthci/oberth/internal/installer"
)

func TestLoadBalancerWaitKeepsReviewedHost(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/api/v1/namespaces/chosen/services/oberth" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"Service","spec":{"type":"LoadBalancer"},"status":{"loadBalancer":{"ingress":[{"ip":"192.0.2.10"}]}}}`))
	}))
	defer server.Close()
	root := t.TempDir()
	configPath := filepath.Join(root, "config")
	planPath := filepath.Join(root, "plan.json")
	config := clientcmdapi.NewConfig()
	config.Clusters["chosen"] = &clientcmdapi.Cluster{Server: server.URL}
	config.Contexts["chosen"] = &clientcmdapi.Context{Cluster: "chosen"}
	config.CurrentContext = "chosen"
	if err := clientcmd.WriteToFile(*config, configPath); err != nil {
		t.Fatal(err)
	}
	plan := installer.InstallPlan{Version: 1, Identity: installer.ClusterIdentity{Files: []string{configPath}, Context: "chosen", Nodes: []installer.NodeIdentity{{Name: "node"}}}, Review: installer.InstallReview{Namespace: "chosen", ClientHost: "reviewed.example.test"}}
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(planPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	old := clientPortsProbeDeps
	defer func() { clientPortsProbeDeps = old }()
	clientPortsProbeDeps = func() installer.ClientPortProbeDeps {
		return installer.ClientPortProbeDeps{Dial: func(context.Context, string) error { return errors.New("fixture") }, LookupHost: func(context.Context, string) ([]string, error) { return []string{"192.0.2.10"}, nil }}
	}
	var out bytes.Buffer
	if err := runClientPorts(t.Context(), []string{"--plan-file", planPath, "--wait-load-balancer", "--wait-timeout", "1s"}, &out); err != nil {
		t.Fatal(err)
	}
	var result installer.ClientPortProbe
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Host != "reviewed.example.test" {
		t.Fatalf("reproduction changed: %+v", result)
	}
	t.Log("reviewed hostname retained through the LoadBalancer wait")
}
