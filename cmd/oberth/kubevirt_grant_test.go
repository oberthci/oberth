package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

func TestKubeVirtGrantCLIReadsDurableRepoGrant(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "oberth.sqlite")
	database, err := store.Open(ctx, path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	upstream, err := database.CreateUpstream(ctx, model.UpstreamSpec{Name: "github", Kind: "ssh", BaseURL: "ssh://github.com/acme"})
	if err != nil {
		t.Fatal(err)
	}
	repository, err := database.CreateRepository(ctx, model.RepositorySpec{Name: "oberth", UpstreamID: upstream.ID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.SetKubeVirtGrant(ctx, repository.ID, "admin@host", true); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := runKubeVirtGrant(ctx, []string{"get", "--database", path, "oberth"}, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"repo_id":`) || !strings.Contains(output.String(), `"granted_by":"admin@host"`) {
		t.Fatalf("grant output = %s", output.String())
	}
}
