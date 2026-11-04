package main

import (
	"strings"
	"testing"
)

func TestConcurrencyGroupsCLIIsReadOnly(t *testing.T) {
	const token = "concurrency-test-token"
	server := newMCPEchoServer(t, token)
	defer server.Close()
	defer withTestMCPClient(t, server)()
	t.Setenv("OBERTH_TOKEN", token)
	t.Setenv("OBERTH_MCP_URL", server.URL)
	var output strings.Builder
	if err := runAdmission(t.Context(), []string{"groups", "--json"}, &output); err != nil || !strings.Contains(output.String(), "tool=concurrency_groups args={}") {
		t.Fatal(output.String(), err)
	}
}
