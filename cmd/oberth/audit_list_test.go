package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestAuditCLIForwardsFiltersToTheMCPTool(t *testing.T) {
	const token = "test-audit-token"
	server := newMCPEchoServer(t, token)
	defer server.Close()
	defer withTestMCPClient(t, server)()
	t.Setenv("OBERTH_TOKEN", token)
	t.Setenv("OBERTH_MCP_URL", server.URL)
	var output strings.Builder
	err := runAudit(context.Background(), []string{"--json", "--action", "secret_access.", "--actor", "agent@host", "--repo", "oberth", "--since", "24h", "--limit", "5", "--before", "40"}, &output)
	if err != nil {
		t.Fatal(err)
	}
	want := `tool=audit_list args={"action":"secret_access.","actor":"agent@host","before":40,"limit":5,"repo":"oberth","since":"24h"}`
	if !strings.Contains(output.String(), want) {
		t.Fatalf("CLI request = %s, want %s", output.String(), want)
	}
	for _, bad := range [][]string{{"--tail", "--before", "3"}, {"--bogus"}, {"stray"}} {
		if err := runAudit(context.Background(), bad, &output); err == nil || (bad[0] != "stray" && !errors.Is(err, errUsage)) {
			t.Fatalf("audit %v = %v, want usage error", bad, err)
		}
	}
}
