package main

import (
	"context"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
)

type listedUpstreams []model.Upstream

func (l listedUpstreams) ListUpstreams(context.Context) ([]model.Upstream, error) { return l, nil }

// --repin keeps an identical registration and refuses to silently change a
// different one under the same name.
func TestSameUpstreamRegisteredKeepsOnlyAnIdenticalRegistration(t *testing.T) {
	t.Parallel()
	spec := model.UpstreamSpec{Name: "forge", Kind: "ssh", BaseURL: "ssh://git@forge.example/~/acme"}
	database := listedUpstreams{{Name: "forge", Kind: "ssh", BaseURL: spec.BaseURL}}
	if registered, err := sameUpstreamRegistered(context.Background(), database, spec); err != nil || !registered {
		t.Fatalf("identical registration = %v, %v", registered, err)
	}
	if registered, err := sameUpstreamRegistered(context.Background(), listedUpstreams{}, spec); err != nil || registered {
		t.Fatalf("missing registration = %v, %v", registered, err)
	}
	moved := spec
	moved.BaseURL = "ssh://git@forge.example/acme"
	if _, err := sameUpstreamRegistered(context.Background(), database, moved); err == nil || !strings.Contains(err.Error(), "upstream remove") {
		t.Fatalf("changed registration error = %v", err)
	}
}
