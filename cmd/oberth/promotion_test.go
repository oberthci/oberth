package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestPromotionRetryCLIUsesMCPTool(t *testing.T) {
	const token = "test-promotion-retry-token"
	server := newMCPEchoServer(t, token)
	defer server.Close()
	defer withTestMCPClient(t, server)()
	t.Setenv("OBERTH_TOKEN", token)
	t.Setenv("OBERTH_MCP_URL", server.URL)
	for _, args := range [][]string{
		{"retry", "0123456789abcdef0123456789abcdef"},
		{"retry", "--json", "0123456789abcdef0123456789abcdef"},
	} {
		var output strings.Builder
		if err := runPromotion(context.Background(), args, &output); err != nil {
			t.Fatalf("promotion %v: %v", args, err)
		}
		if !strings.Contains(output.String(), `tool=promote_retry args={"id":"0123456789abcdef0123456789abcdef"}`) {
			t.Fatalf("CLI request = %s", output.String())
		}
	}
	var output strings.Builder
	for _, args := range [][]string{nil, {"retry"}, {"replay", "x"}} {
		if err := runPromotion(context.Background(), args, &output); !errors.Is(err, errUsage) {
			t.Fatalf("promotion %v error = %v, want usage", args, err)
		}
	}
}
