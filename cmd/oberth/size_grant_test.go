package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

func TestSizeGrantCLIReadsAndValidates(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "oberth.sqlite")
	database, err := store.Open(ctx, path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	upstream, err := database.CreateUpstream(ctx, model.UpstreamSpec{Name: "codeberg", Kind: "ssh", BaseURL: "ssh://codeberg.org/acme"})
	if err != nil {
		t.Fatal(err)
	}
	repository, err := database.CreateRepository(ctx, model.RepositorySpec{Name: "cloudtaser-ebpf", UpstreamID: upstream.ID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.SetSizeGrant(ctx, repository.ID, "admin@host", "XL"); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := runSizeGrant(ctx, []string{"get", "cloudtaser-ebpf", "--database", path}, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"max_size":"XL"`) || !strings.Contains(output.String(), `"granted_by":"admin@host"`) {
		t.Fatalf("size get output = %s", output.String())
	}
	for _, arguments := range [][]string{
		{"grant", "cloudtaser-ebpf", "--database", path},
		{"grant", "cloudtaser-ebpf", "--max", "M", "--database", path},
		{"grant", "--max", "XL"},
		{"resize", "cloudtaser-ebpf"},
		{},
	} {
		if err := runSizeGrant(ctx, arguments, &output); !errors.Is(err, errUsage) {
			t.Fatalf("%v: %v, want a usage error", arguments, err)
		}
	}
}
