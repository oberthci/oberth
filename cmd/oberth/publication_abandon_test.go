package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func TestPublicationAbandonCLI(t *testing.T) {
	const token = "test-publication-abandon"
	server := newMCPEchoServer(t, token)
	defer server.Close()
	defer withTestMCPClient(t, server)()
	t.Setenv("OBERTH_TOKEN", token)
	t.Setenv("OBERTH_MCP_URL", server.URL)
	var output bytes.Buffer
	if err := runCLI(context.Background(), []string{"publication", "abandon", "0123456789abcdef0123456789abcdef", "--reason", "stop delivery", "--json"}, nil, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `tool=publication_abandon args={"id":"0123456789abcdef0123456789abcdef","reason":"stop delivery"}`) {
		t.Fatalf("CLI request: %s", output.String())
	}
	for _, bad := range [][]string{
		{}, {"abandon"}, {"abandon", "id"}, {"abandon", "id", "--reason", "x", "--force"},
		{"abandon", "id", "--reason", "x", "--database", "unused.sqlite"},
		{"abandon", "id", "--reason", "two\nlines"},
	} {
		if err := runPublication(context.Background(), bad, &output); !errors.Is(err, errUsage) {
			t.Fatalf("%v: %v", bad, err)
		}
	}
}
