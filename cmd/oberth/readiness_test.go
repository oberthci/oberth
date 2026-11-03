package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/oberthci/oberth/internal/model"
)

func TestReadinessConfigurationUsesSampledDedicatedKey(t *testing.T) {
	directory := t.TempDir()
	options := serveOptions{upstreamKey: filepath.Join(directory, "missing-shared"), knownHosts: filepath.Join(directory, "known_hosts")}
	for _, path := range []string{filepath.Join(directory, "dedicated"), options.knownHosts} {
		if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	probe := upstreamReadinessProbe(options)
	if err := probe(t.Context(), []model.Upstream{{Kind: "ssh", KeyName: "dedicated"}}); err != nil {
		t.Fatal(err)
	}
	if err := upstreamReadinessProbe(options)(t.Context(), nil); err == nil {
		t.Fatal("credential check ignored the sampled upstream rows")
	}
}

func TestReadinessIdentityProbePreservesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := upstreamReadinessProbe(serveOptions{})(ctx, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled probe: %v", err)
	}
	if err := upstreamReadinessProbe(serveOptions{})(t.Context(), nil); err == nil {
		t.Fatal("missing materialized identities must refuse readiness")
	}
}
