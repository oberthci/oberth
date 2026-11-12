package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
)

func TestPipelineNetworkPolicyStatus(t *testing.T) {
	enabled, disabled := true, false
	for _, tc := range []struct {
		name       string
		configured *bool
		detail     string
	}{
		{"enabled", &enabled, "pipeline NetworkPolicy is configured enabled"},
		{"disabled", &disabled, "WARNING: pipeline NetworkPolicy is configured disabled; Oberth does not configure pipeline egress isolation"},
		{"unavailable", nil, "pipeline NetworkPolicy configuration is unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			health := Health{
				PipelineNetworkPolicyEnabled: tc.configured,
				Store:                        fakeHealthStore{upstreams: []model.Upstream{{ID: 1, Name: "visible", Kind: "local"}, {ID: 2, Name: "hidden", Kind: "local"}}, repositories: []model.Repository{{ID: 1, UpstreamID: 1}, {ID: 2, UpstreamID: 2}}},
				Configured:                   func(context.Context, []model.Upstream) error { return nil },
				Cluster:                      func(context.Context) error { return nil }, Audit: func(context.Context) error { return nil },
				VCS:         func(context.Context, model.Upstream) error { return nil },
				SecretStore: &SecretStoreStatus{Configured: true, Address: "https://store.invalid"},
			}
			for _, scoped := range []bool{false, true} {
				for repeat := 0; repeat < 2; repeat++ {
					var value any
					var err error
					if scoped {
						value, err = sampleHealth(health, t.Context()).ScopedStatus(t.Context(), api.Actor{Scope: []string{"visible"}})
					} else {
						value, err = sampledStatus(health, t.Context())
					}
					if err != nil {
						t.Fatal(err)
					}
					status := value.(HealthStatus)
					if status.ClusterDetail != tc.detail {
						t.Fatalf("scoped=%v detail=%q, want %q", scoped, status.ClusterDetail, tc.detail)
					}
					if status.Cluster != "ready" || status.Database != "ready" || status.VCS != "ready" || status.Audit != "ready" {
						t.Fatalf("configuration changed dependency health: %+v", status)
					}
					want := 2
					if scoped {
						want = 1
						if status.SecretStore != nil {
							t.Fatal("scoped status disclosed secret-store configuration")
						}
					}
					if status.Upstreams != want || status.Repositories != want {
						t.Fatalf("scoped=%v counts=%+v", scoped, status)
					}
					payload, err := json.Marshal(status)
					if err != nil {
						t.Fatal(err)
					}
					if !strings.Contains(string(payload), `"cluster_detail":"`+tc.detail+`"`) {
						t.Fatalf("diagnostic absent from wire status: %s", payload)
					}
				}
			}
			if err := health.Ready(t.Context()); err != nil {
				t.Fatalf("configuration gated readiness: %v", err)
			}
			for _, store := range []HealthStore{nil, fakeHealthStore{err: errors.New("database unavailable")}} {
				health.Store = store
				health.Cluster = func(context.Context) error { return errors.New("cluster unavailable") }
				for _, scoped := range []bool{false, true} {
					var value any
					if scoped {
						value, _ = sampleHealth(health, t.Context()).ScopedStatus(t.Context(), api.Actor{})
					} else {
						value, _ = sampledStatus(health, t.Context())
					}
					status := value.(HealthStatus)
					if status.ClusterDetail != tc.detail || status.Cluster != "unavailable" || status.Database != "unavailable" {
						t.Fatalf("outage hid configuration: %+v", status)
					}
				}
			}
		})
	}
}
