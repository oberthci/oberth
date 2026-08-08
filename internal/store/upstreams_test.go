package store

import (
	"context"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestUpstreamCatalogListsDeterministicallyAndFindsByName(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 4, 20, 0, 0, 0, time.UTC)
	database := testStore(t, &now)
	for _, spec := range []model.UpstreamSpec{
		{Name: "zeta", Kind: "ssh", BaseURL: "ssh://git@example.invalid/zeta"},
		{Name: "alpha", Kind: "ssh", BaseURL: "ssh://git@example.invalid/alpha"},
	} {
		if _, err := database.CreateUpstream(context.Background(), spec); err != nil {
			t.Fatal(err)
		}
	}
	values, err := database.ListUpstreams(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 2 || values[0].Name != "alpha" || values[1].Name != "zeta" {
		t.Fatalf("upstreams = %+v", values)
	}
	alpha, err := database.UpstreamByName(context.Background(), "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if alpha.Name != "alpha" || alpha.BaseURL != "ssh://git@example.invalid/alpha" {
		t.Fatalf("alpha = %+v", alpha)
	}
}
