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
	if _, err := database.SetKubeVirtGrant(ctx, repository.ID, "admin@host", true, store.KubeVirtBudget{}); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := runKubeVirtGrant(ctx, []string{"get", "--database", path, "oberth"}, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"repo_id":`) || !strings.Contains(output.String(), `"granted_by":"admin@host"`) ||
		!strings.Contains(output.String(), `"max_vms":2,"max_cores":6,"max_memory_bytes":12884901888`) {
		t.Fatalf("grant output = %s", output.String())
	}
}

func TestKubeVirtGrantCLIBudgetFlags(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"revoke takes no budget", []string{"revoke", "oberth", "--max-vms", "3"}, "flag provided but not defined"},
		{"get takes no budget", []string{"get", "--max-cores", "3", "oberth"}, "flag provided but not defined"},
		{"too many VMs", []string{"grant", "oberth", "--max-vms", "17"}, "max VMs must be 1..16"},
		{"bad memory", []string{"grant", "oberth", "--max-memory", "lots"}, "KubeVirt max memory"},
		{"memory too large", []string{"grant", "oberth", "--max-memory", "300Gi"}, "1Gi..256Gi"},
		{"extra argument", []string{"grant", "oberth", "other"}, "kubevirt grant <repo>"},
		{"missing repo", []string{"grant", "--max-vms", "3"}, "kubevirt grant <repo>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := runKubeVirtGrant(ctx, append(tc.args, "--database", filepath.Join(t.TempDir(), "absent.sqlite")), &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want usage error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestKubeVirtCeilingFlags(t *testing.T) {
	ceiling, err := kubeVirtCeiling(serveOptions{})
	if err != nil || ceiling.MaxVMs != 8 || ceiling.MaxCores != 24 || ceiling.MaxMemory.String() != "48Gi" {
		t.Fatalf("default ceiling = %v, %v", ceiling, err)
	}
	ceiling, err = kubeVirtCeiling(serveOptions{argoKubeVirtMaxVMs: 5, argoKubeVirtMaxCores: 10, argoKubeVirtMaxMemory: "40Gi"})
	if err != nil || ceiling.MaxVMs != 5 || ceiling.MaxCores != 10 || ceiling.MaxMemory.String() != "40Gi" {
		t.Fatalf("configured ceiling = %v, %v", ceiling, err)
	}
	for _, options := range []serveOptions{{argoKubeVirtMaxVMs: 17}, {argoKubeVirtMaxCores: 65}, {argoKubeVirtMaxMemory: "lots"}, {argoKubeVirtMaxMemory: "512Gi"}} {
		if _, err := kubeVirtCeiling(options); err == nil {
			t.Errorf("ceiling %+v accepted", options)
		}
	}
}
