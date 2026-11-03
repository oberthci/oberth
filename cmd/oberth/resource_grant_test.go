package main

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

func TestResourceGrantCLIReadsAndValidates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oberth.sqlite")
	database, err := store.Open(t.Context(), path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	up, err := database.CreateUpstream(t.Context(), model.UpstreamSpec{Name: "example", Kind: "ssh", BaseURL: "ssh://example.invalid/org"})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := database.CreateRepository(t.Context(), model.RepositorySpec{Name: "repo-01", UpstreamID: up.ID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	ceiling, err := store.ParseResourceCeiling(map[string]string{"cpu": "1250m", "memory": "2Gi"}, map[string]string{"cpu": "3", "memory": "5Gi"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = database.SetResourceCeiling(t.Context(), repo.ID, "admin", &ceiling); err != nil {
		t.Fatal(err)
	}
	if err = database.Close(); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err = runResourceGrant(t.Context(), []string{"get", "repo-01", "--database", path}, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"cpu":"1250m"`) || !strings.Contains(output.String(), `"granted_by":"admin"`) {
		t.Fatalf("wrong numeric grant: %s", output.String())
	}
	for _, args := range [][]string{
		{}, {"unknown", "repo-01"}, {"grant", "repo-01"},
		{"grant", "repo-01", "--requests-cpu", "2", "--requests-memory", "2Gi", "--limits-cpu", "1", "--limits-memory", "4Gi"},
		{"grant", "repo-01", "--requests-cpu", "bad"},
	} {
		if err = runResourceGrant(t.Context(), args, &output); !errors.Is(err, errUsage) {
			t.Fatalf("invalid usage %v: %v", args, err)
		}
	}
}
