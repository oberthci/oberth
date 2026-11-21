// Package contracttest checks the installer chart port's read, preview and apply laws.
//
// Spec: docs/plans/c9-install-and-upgrade.md#owned-data-and-public-contract
package contracttest

import (
	"context"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"helm.sh/helm/v3/pkg/chart"
)

func Chart(t *testing.T, newPort func() installer.ChartPort) {
	t.Helper()
	t.Run("preview is read only and apply retains explicit values", func(t *testing.T) {
		ctx := context.Background()
		p := newPort()
		h, err := p.Read(ctx, "oberth")
		if err != nil || h.Latest.Number != 0 {
			t.Fatal(h, err)
		}
		ch := &chart.Chart{
			Metadata: &chart.Metadata{Name: "oberth", Version: "1.0.0", AppVersion: "1.0.0", APIVersion: "v2"},
			Values:   map[string]any{"enabled": true, "parallel": 4},
			Schema:   []byte(`{"type":"object","additionalProperties":false,"properties":{"enabled":{"type":"boolean"},"parallel":{"type":"integer"},"old":{"type":"boolean"},"global":{"type":"object","additionalProperties":false}}}`),
		}
		r := installer.ChartRequest{
			Chart:     ch,
			Namespace: "oberth",
			Values:    map[string]any{"enabled": false, "parallel": 0, "old": true},
		}
		if err = p.Preview(ctx, r); err != nil {
			t.Fatal(err)
		}
		h, err = p.Read(ctx, "oberth")
		if err != nil || h.Latest.Number != 0 {
			t.Fatal("preview wrote history", h, err)
		}
		if err = p.Apply(ctx, r); err != nil {
			t.Fatal(err)
		}
		h, err = p.Read(ctx, "oberth")
		if err != nil || h.Deployed.Number != 1 || !h.Deployed.Installer() || h.Deployed.Values["enabled"] != false {
			t.Fatal(h, err)
		}
		upgrade(t, p, r)
		r.Values["unknown"] = true
		if err = p.Preview(ctx, r); err == nil {
			t.Fatal("unknown value accepted")
		}
	})
}

func upgrade(t *testing.T, p installer.ChartPort, r installer.ChartRequest) {
	t.Helper()
	ctx := t.Context()
	h, err := p.Read(ctx, r.Namespace)
	if err != nil {
		t.Fatal(err)
	}
	r.Revision, r.Predecessor, r.ManifestDigest = h.Latest.Number, h.Deployed.Number, h.Deployed.ManifestDigest
	r.Values = map[string]any{"enabled": true, "parallel": 1}
	if err := p.Preview(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := p.Apply(ctx, r); err != nil {
		t.Fatal(err)
	}
	h, err = p.Read(ctx, "oberth")
	if err != nil || h.Deployed.Number != 2 || h.Deployed.Values["enabled"] != true || h.Deployed.Values["parallel"] != 1 {
		t.Fatal("upgrade did not use complete new values", h, err)
	}
	if _, ok := h.Deployed.Values["old"]; ok {
		t.Fatal("upgrade reused old values", h)
	}
}
