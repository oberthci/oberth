package main

import (
	"context"
	"errors"
	"log"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/argojob"
)

func TestRepoArchiveCLIForwardsToAdminMCP(t *testing.T) {
	const token = "test-archive-token"
	server := newMCPEchoServer(t, token)
	defer server.Close()
	defer withTestMCPClient(t, server)()
	t.Setenv("OBERTH_TOKEN", token)
	t.Setenv("OBERTH_MCP_URL", server.URL)
	for _, action := range []string{"archive", "unarchive"} {
		var output strings.Builder
		if err := runRepoWithDependencies(context.Background(), []string{action, "github/org/repo"}, &output, repoDependencies{}); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(output.String(), `tool=repo_`+action+` args={"repo":"github/org/repo"}`) {
			t.Fatalf("request = %s", output.String())
		}
		for _, bad := range [][]string{{}, {"a", "b"}, {"a", "--database", "/tmp/db"}, {"a", "--force"}} {
			if err := runRepoArchive(context.Background(), action, bad, &output); !errors.Is(err, errUsage) {
				t.Fatalf("%v = %v", bad, err)
			}
		}
	}
}

func TestArchivedReleaseWIFWarningNamesStaleMapping(t *testing.T) {
	var output strings.Builder
	logger := log.New(&output, "", 0)
	policy := &argojob.ReleaseWIFConfig{Repositories: map[string]argojob.ReleaseWIFRepository{"github/org/retired": {}}}
	warnArchivedReleaseWIF(logger, policy, "github/org/active")
	warnArchivedReleaseWIF(logger, nil, "github/org/retired")
	if output.Len() != 0 {
		t.Fatal("warning for absent capability")
	}
	warnArchivedReleaseWIF(logger, policy, "github/org/retired")
	if !strings.Contains(output.String(), "releaseWIF entry for archived repository github/org/retired must be removed") {
		t.Fatal(output.String())
	}
}
