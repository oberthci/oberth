package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
)

func TestClientPortsUsesPublicPlanAndExplicitEndpoints(t *testing.T) {
	old := clientPortsProbeDeps
	t.Cleanup(func() { clientPortsProbeDeps = old })
	addresses := []string{}
	clientPortsProbeDeps = func() installer.ClientPortProbeDeps {
		return installer.ClientPortProbeDeps{Dial: func(_ context.Context, address string) error {
			addresses = append(addresses, address)
			return errors.New("not yet listening")
		}}
	}
	plan := installer.InstallPlan{Version: 1, Identity: installer.ClusterIdentity{Context: "chosen", Nodes: []installer.NodeIdentity{{Name: "node"}}}, Review: installer.InstallReview{ClientHost: "localhost"}}
	raw, _ := json.Marshal(plan)
	path := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := runClientPorts(t.Context(), []string{"--plan-file", path, "--client-https-port", "32443", "--client-ssh-port", "32022"}, &output); err != nil {
		t.Fatal(err)
	}
	var result installer.ClientPortProbe
	if err := json.Unmarshal(output.Bytes(), &result); err != nil || result.Context != "chosen" || !result.NeedsAction || strings.Join(addresses, ",") != "localhost:32443,localhost:32022" {
		t.Fatalf("%+v %v", result, err)
	}
	if err := os.WriteFile(path, []byte("public-file-content-not-for-errors"), 0600); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	err := runClientPorts(t.Context(), []string{"--plan-file", path}, &output)
	if err == nil || strings.Contains(err.Error(), "public-file-content-not-for-errors") || output.Len() != 0 {
		t.Fatal("invalid metadata echoed or accepted")
	}
}
