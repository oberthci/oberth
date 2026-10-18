package app

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
)

// An upstream-scoped uplink's status counts and shows only its upstreams
// (#073): nothing in it may reveal the other upstreams or their volume.
func TestScopedStatusCountsOnlyTheScope(t *testing.T) {
	t.Parallel()
	health := Health{
		Store: fakeHealthStore{
			upstreams: []model.Upstream{
				{ID: 1, Name: "github", Kind: "ssh", BaseURL: "ssh://git@github.com/oberthci"},
				{ID: 2, Name: "codeberg", Kind: "ssh", BaseURL: "ssh://git@codeberg.org/cloudtaser"},
			},
			repositories: []model.Repository{
				{ID: 1, Name: "oberth", UpstreamID: 1},
				{ID: 2, Name: "cloudtaser-cli", UpstreamID: 2}, {ID: 3, Name: "cloudtaser-wire", UpstreamID: 2},
			},
		},
		Cluster: func(context.Context) error { return nil },
		Audit:   func(context.Context) error { return nil },
		VCS: func(_ context.Context, upstream model.Upstream) error {
			if upstream.Name == "codeberg" {
				return errors.New("codeberg down")
			}
			return nil
		},
		AuditChain: func(context.Context) (AuditChainStatus, error) { return AuditChainStatus{}, nil },
	}
	value, err := health.ScopedStatus(context.Background(), api.Actor{Identity: "laptop@work", Scope: []string{"github"}})
	if err != nil {
		t.Fatal(err)
	}
	status := value.(HealthStatus)
	if status.Upstreams != 1 || status.Repositories != 1 || len(status.UpstreamInfo) != 1 ||
		status.UpstreamInfo[0].Name != "github" || status.VCS != "ready" || status.AuditChain != nil || status.SecretStore != nil {
		t.Fatalf("scoped status = %#v", status)
	}
	encoded, _ := json.Marshal(status)
	if strings.Contains(string(encoded), "codeberg") || strings.Contains(string(encoded), "cloudtaser") {
		t.Fatalf("scoped status leaks the other upstream: %s", encoded)
	}
	// The unscoped view is unchanged: both upstreams, the codeberg outage.
	full, err := health.Status(context.Background())
	if err != nil || full.(HealthStatus).Upstreams != 2 || full.(HealthStatus).Repositories != 3 || full.(HealthStatus).VCS == "ready" {
		t.Fatalf("unscoped status = %#v, %v", full, err)
	}
}

func TestSSHIdentitiesReturnsTheScope(t *testing.T) {
	t.Parallel()
	identities := SSHIdentities{Uplinks: fakeUplinkCatalog{uplink: model.Uplink{Identity: "laptop@work", Scope: []string{"github"}}}}
	identity, scope, found, err := identities.ResolveFingerprintScope("SHA256:key")
	if err != nil || !found || identity != "laptop@work" || !slices.Equal(scope, []string{"github"}) {
		t.Fatalf("resolve = %q, %v, %v, %v", identity, scope, found, err)
	}
}

// A short SSH path that is ambiguous across the installation but unique in a
// key's scope resolves within the scope (#073); refusals name nothing else.
func TestQualifyInScopeResolvesShortNamesWithinTheScope(t *testing.T) {
	t.Parallel()
	catalog := fakeUpstreamCatalog{
		repositories: map[string]model.Repository{
			"github/oberthci/terraform":     {Name: "terraform", UpstreamID: 1},
			"codeberg/cloudtaser/terraform": {Name: "terraform", UpstreamID: 2},
			"codeberg/cloudtaser/secret":    {Name: "secret", UpstreamID: 2},
			"secret":                        {Name: "secret", UpstreamID: 2},
		},
		ambiguous: map[string]bool{"terraform": true},
		upstreams: []model.Upstream{
			{ID: 1, Name: "github", BaseURL: "ssh://git@github.com/oberthci"},
			{ID: 2, Name: "codeberg", BaseURL: "ssh://git@codeberg.org/cloudtaser"},
			{ID: 3, Name: "skipops", BaseURL: "ssh://git@codeberg.org/skipops"},
		},
	}
	resolver := Upstreams{Catalog: catalog}
	for input, want := range map[string]string{
		"terraform":                 "github/oberthci/terraform",
		"oberthci/terraform":        "github/oberthci/terraform",
		"github/oberthci/terraform": "github/oberthci/terraform",
		"new-repo":                  "github/oberthci/new-repo",
		// Registered only outside the scope: a short name still means the
		// scope, exactly as for a name registered nowhere.
		"secret": "github/oberthci/secret",
	} {
		if got, err := resolver.QualifyInScope(input, []string{"github"}); err != nil || got != want {
			t.Errorf("QualifyInScope(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	for _, input := range []string{"cloudtaser/secret", "codeberg/cloudtaser/terraform"} {
		if got, err := resolver.QualifyInScope(input, []string{"github"}); err == nil || strings.Contains(err.Error(), "codeberg") && !strings.Contains(input, "codeberg") {
			t.Errorf("QualifyInScope(%q) = %q, %v; want a refusal naming nothing outside the scope", input, got, err)
		}
	}
	// Two in-scope upstreams: a registered repository decides; a new name
	// that either could hold is refused rather than guessed.
	if got, err := resolver.QualifyInScope("terraform", []string{"github", "skipops"}); err != nil || got != "github/oberthci/terraform" {
		t.Errorf("registered short name across two scopes = %q, %v", got, err)
	}
	if got, err := resolver.QualifyInScope("brand-new", []string{"github", "skipops"}); err == nil {
		t.Errorf("unregistered short name across two scopes = %q, want refused", got)
	}
}
