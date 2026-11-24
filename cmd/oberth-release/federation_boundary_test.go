package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRegistryFederationRefusesMalformedResources(t *testing.T) {
	const provider = "projects/123/locations/global/workloadIdentityPools/pool/providers/provider"
	const account = "writer@project.iam.gserviceaccount.com"
	for _, tc := range []struct{ name, provider, account string }{
		{"provider URL", "https://example.invalid/" + provider, account},
		{"provider project", "projects/project/locations/global/workloadIdentityPools/pool/providers/provider", account},
		{"provider path", provider + "/other", account},
		{"provider query", provider + "?audience=other", account},
		{"account path", provider, "other/" + account},
		{"account method", provider, account + ":generateAccessToken?scope=other"},
		{"account domain", provider, "writer@example.invalid"},
		{"account whitespace", provider, account + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "registry")
			_, _, err := registryIdentity([]string{"--provider", tc.provider, "--service-account", tc.account, "--identity-file", "subject", "--dir", dir})
			if !errors.Is(err, federationConfig) {
				t.Fatal("malformed federation resource was accepted", err)
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatal("invalid configuration created a login")
			}
		})
	}
}

func TestRegistryLoginRequiresFreshPrivateDirectory(t *testing.T) {
	for _, link := range []bool{false, true} {
		name := "directory"
		if link {
			name = "symlink"
		}
		t.Run(name, func(t *testing.T) {
			existing := t.TempDir()
			dir := existing
			if link {
				dir = filepath.Join(t.TempDir(), "registry")
				if err := os.Symlink(existing, dir); err != nil {
					t.Fatal(err)
				}
			}
			if err := writeRegistryLogin(dir, "registry-fixture"); !errors.Is(err, federationDirectory) {
				t.Fatal("existing login directory was reused", err)
			}
			if _, err := os.Stat(filepath.Join(existing, "config.json")); !os.IsNotExist(err) {
				t.Fatal("refused directory received a login")
			}
		})
	}
}

func TestRegistryFederationReasonsNameTheirStage(t *testing.T) {
	seen := map[string]bool{}
	for _, reason := range []refusal{federationConfig, federationExchange, federationImpersonation, federationDirectory} {
		if len(reason) > 40 || len(reason.Remedy()) == 0 || len(reason.Remedy()) > 120 || seen[reason.Error()] {
			t.Fatal("federation stage is missing, repeated or unbounded", reason)
		}
		seen[reason.Error()] = true
	}
}
