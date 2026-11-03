package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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

func TestPromotionCreateCLITrain(t *testing.T) {
	const token = "test-train-token"
	server := newMCPEchoServer(t, token)
	defer server.Close()
	defer withTestMCPClient(t, server)()
	t.Setenv("OBERTH_TOKEN", token)
	t.Setenv("OBERTH_MCP_URL", server.URL)
	sources := `[{"sha":"1111111111111111111111111111111111111111","ref":"feature/one"},{"sha":"2222222222222222222222222222222222222222"},{"sha":"3333333333333333333333333333333333333333"}]`
	path := filepath.Join(t.TempDir(), "sources.json")
	if err := os.WriteFile(path, []byte(sources), 0600); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := runPromotion(t.Context(), []string{"create", "--branch", "main", "--repo", "codeberg.org/acme/widgets", "--sources-file", path, "--skip-conflicting", "--json"}, &out); err != nil {
		t.Fatal(err)
	}
	var canonical any
	if err := json.Unmarshal([]byte(sources), &canonical); err != nil {
		t.Fatal(err)
	}
	canonicalSources, err := json.Marshal(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "tool=promote ") || !strings.Contains(out.String(), `"sources":`+string(canonicalSources)) || !strings.Contains(out.String(), `"skip_conflicting":true`) {
		t.Fatal(out.String())
	}
	for _, args := range [][]string{{"create", "--branch", "main"}, {"create", "--branch", "main", "--sha", "abc", "--sources-file", path}, {"create", "--branch", "main", "--sha", "abc", "--skip-conflicting"}} {
		if err := runPromotion(t.Context(), args, &out); !errors.Is(err, errUsage) {
			t.Fatalf("%v: %v", args, err)
		}
	}
}
