package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestRunPurgeCLIForwardsToMCP(t *testing.T) {
	const token = "test-purge-token"
	server := newMCPEchoServer(t, token)
	defer server.Close()
	defer withTestMCPClient(t, server)()
	t.Setenv("OBERTH_TOKEN", token)
	t.Setenv("OBERTH_MCP_URL", server.URL)
	var output strings.Builder
	if err := runRunDetail(context.Background(), []string{"purge", "0123456789ab", "--what=both", "--reason", "retention cleanup", "--json"}, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `tool=run_purge args={"id":"0123456789ab","reason":"retention cleanup","what":"both"}`) {
		t.Fatalf("CLI=%s", output.String())
	}
	for _, arguments := range [][]string{{"purge"}, {"purge", "0123456789ab", "--what=both"}, {"purge", "0123456789ab", "--what=all", "--reason=x"}, {"purge", "0123456789ab", "--what=both", "--reason=x", "--bogus"}} {
		if err := runRunDetail(context.Background(), arguments, &output); !errors.Is(err, errUsage) {
			t.Fatalf("%v=%v", arguments, err)
		}
	}
}
