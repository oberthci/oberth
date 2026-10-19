package main

import (
	"context"
	"strings"
	"testing"
)

func TestRunRetryCLIUsesMCPTool(t *testing.T) {
	const token = "test-retry-token"
	server := newMCPEchoServer(t, token)
	defer server.Close()
	defer withTestMCPClient(t, server)()
	t.Setenv("OBERTH_TOKEN", token)
	t.Setenv("OBERTH_MCP_URL", server.URL)
	var output strings.Builder
	if err := runRunDetail(context.Background(), []string{"retry", "0123456789abcdef0123456789abcdef"}, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `tool=run_retry args={"id":"0123456789abcdef0123456789abcdef"}`) {
		t.Fatalf("CLI request = %s", output.String())
	}
	for _, args := range [][]string{
		{"retry", "--json", "0123456789abcdef0123456789abcdef"},
		{"retry", "0123456789abcdef0123456789abcdef", "--json"},
	} {
		output.Reset()
		if err := runRunDetail(context.Background(), args, &output); err != nil {
			t.Fatalf("--json retry %v: %v", args, err)
		}
		if !strings.Contains(output.String(), `tool=run_retry args={"id":"0123456789abcdef0123456789abcdef"}`) {
			t.Fatalf("--json CLI request = %s", output.String())
		}
	}
}
