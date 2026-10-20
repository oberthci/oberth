package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestRunRerunCLIForwardsToTheMCPTool(t *testing.T) {
	const token = "test-rerun-token"
	server := newMCPEchoServer(t, token)
	defer server.Close()
	defer withTestMCPClient(t, server)()
	t.Setenv("OBERTH_TOKEN", token)
	t.Setenv("OBERTH_MCP_URL", server.URL)
	var output strings.Builder
	if err := runRunDetail(context.Background(), []string{"rerun", "0123456789abcdef0123456789abcdef", "--reason", "flaky test", "--force"}, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `tool=run_rerun args={"force":true,"id":"0123456789abcdef0123456789abcdef","reason":"flaky test"}`) {
		t.Fatalf("CLI request = %s", output.String())
	}
	output.Reset()
	if err := runRunDetail(context.Background(), []string{"rerun", "--json", "0123456789ab"}, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `tool=run_rerun args={"id":"0123456789ab"}`) {
		t.Fatalf("--json CLI request = %s", output.String())
	}
	for _, bad := range [][]string{
		{"rerun"},
		{"rerun", "a", "b"},
		{"rerun", "0123456789ab", "--bogus"},
		{"rerun", "0123456789ab", "--database", "/data/oberth.sqlite"},
	} {
		if err := runRunDetail(context.Background(), bad, &output); !errors.Is(err, errUsage) {
			t.Fatalf("run %v = %v, want usage error", bad, err)
		}
	}
}
