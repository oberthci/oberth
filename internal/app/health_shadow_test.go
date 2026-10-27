package app

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/gitcache"
)

func TestHealthShadowCountersAreLiveAndInstallationScoped(t *testing.T) {
	stats := gitcache.ShadowStats{Runs: 12, Mismatches: 2, Errors: 1, Skipped: 3}
	health := Health{GitcacheShadow: func() gitcache.ShadowStats { return stats }}
	for _, runs := range []uint64{12, 13} {
		stats.Runs = runs
		value, err := health.Status(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		body, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var got gitcache.ShadowStats
		if err := json.Unmarshal(body, &got); err != nil || got != stats {
			t.Fatalf("status counters: %+v %s %v", got, body, err)
		}
	}
	value, err := health.ScopedStatus(t.Context(), api.Actor{Identity: "scoped", Scope: []string{"github"}})
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(value)
	if err != nil || strings.Contains(string(body), "shadow_") {
		t.Fatalf("scoped actor saw global counters: %s %v", body, err)
	}
}
