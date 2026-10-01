package main

import (
	"context"
	"io"
	"path/filepath"
	"testing"

	"github.com/oberthci/oberth/internal/goproxy"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

func TestGoProxyDiscoveryBindsExactCatalogOwner(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "instance.sqlite"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	config := goproxy.NamespaceConfig{ModulePrefix: "go.example.test", RepositoryPrefix: "sample-", Upstream: "forge", Organization: "example"}
	empty, err := buildGoProxyRepoMap(ctx, db, config)
	if err != nil || len(empty) != 0 {
		t.Fatalf("unenrolled namespace: %v %v", empty, err)
	}
	for _, spec := range []model.UpstreamSpec{{Name: "forge", Kind: "ssh", BaseURL: "ssh://git@forge.example.test/example"}, {Name: "other", Kind: "ssh", BaseURL: "ssh://git@other.example.test/untrusted"}} {
		owner, err := db.CreateUpstream(ctx, spec)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"sample-wire", "unmatched", "sample-"} {
			if _, err := db.CreateRepository(ctx, model.RepositorySpec{Name: name, UpstreamID: owner.ID, DefaultBranch: "main"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	got, err := buildGoProxyRepoMap(ctx, db, config)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got["wire"] != "forge/example/sample-wire" {
		t.Fatalf("cross-owner or unmatched repository routed: %v", got)
	}
	wrong := config
	wrong.Organization = "untrusted"
	if _, err := buildGoProxyRepoMap(ctx, db, wrong); err == nil {
		t.Fatal("organization mismatch accepted")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := buildGoProxyRepoMap(ctx, db, config); err == nil {
		t.Fatal("catalog failure silently became empty routing")
	}
}

func TestGoProxyServeRequiresExplicitNamespaceAuthority(t *testing.T) {
	args := []string{"--argo-namespace=argo-pipelines", "--argo-goproxy-listen=:8444", "--argo-goproxy-cert=/etc/proxy/tls.crt", "--argo-goproxy-key=/etc/proxy/tls.key", "--argo-goproxy-ca=/etc/proxy/ca.crt", "--argo-goproxy-url=https://proxy.example.test:8444"}
	if _, err := parseServeOptions(args, io.Discard); err == nil {
		t.Fatal("enabled proxy accepted implicit namespace authority")
	}
	args = append(args, "--argo-goproxy-module-prefix=go.example.test", "--argo-goproxy-repository-prefix=sample-", "--argo-goproxy-upstream=forge", "--argo-goproxy-organization=example")
	options, err := parseServeOptions(args, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if options.argoGoProxyNamespace.ModulePrefix != "go.example.test" || options.argoGoProxyNamespace.Upstream != "forge" || options.argoGoProxyNamespace.Organization != "example" || options.argoGoProxyNamespace.RepositoryPrefix != "sample-" {
		t.Fatal("explicit proxy authority did not survive command parsing")
	}
}
