package main

import (
	"errors"
	"strings"
	"testing"
)

func TestRuntimeAdminCLIForwardsAuditedTools(t *testing.T) {
	const token = "runtime-admin-test-token"
	server := newMCPEchoServer(t, token)
	defer server.Close()
	defer withTestMCPClient(t, server)()
	t.Setenv("OBERTH_TOKEN", token)
	t.Setenv("OBERTH_MCP_URL", server.URL)
	for _, test := range []struct {
		args []string
		want string
	}{
		{[]string{"list"}, `tool=release_wif_list args={}`},
		{[]string{"grant", "github/acme/app", "--service-account", "app-release", "--template", "publish=image-writer,sign=image-reader", "--reason", "absorb"}, `tool=release_wif_grant args={"reason":"absorb","repo":"github/acme/app","service_account_name":"app-release","templates":{"publish":"image-writer","sign":"image-reader"}}`},
		{[]string{"revoke", "github/retired/old", "--reason", "deleted upstream"}, `tool=release_wif_revoke args={"reason":"deleted upstream","repo":"github/retired/old"}`},
		{[]string{"reset", "github/retired/old", "--reason", "back to helm"}, `tool=release_wif_reset args={"reason":"back to helm","repo":"github/retired/old"}`},
	} {
		var output strings.Builder
		err := runReleaseWIF(t.Context(), test.args, &output)
		if err != nil || !strings.Contains(output.String(), test.want) {
			t.Fatalf("%v = %s, %v", test.args, output.String(), err)
		}
	}
	for _, args := range [][]string{
		{}, {"drop"}, {"list", "repo"}, {"grant", "repo", "--reason", "x", "--template", "a=b"},
		{"grant", "repo", "--reason", "x", "--service-account", "sa", "--template", "a"},
		{"grant", "repo", "--reason", "x", "--service-account", "sa", "--template", "a=b,a=c"},
		{"revoke", "repo"}, {"revoke", "repo", "--reason", "x", "--service-account", "sa"},
	} {
		if err := runReleaseWIF(t.Context(), args, &strings.Builder{}); !errors.Is(err, errUsage) {
			t.Fatal(args, err)
		}
	}
}
