package installer

import (
	"strings"
	"testing"
)

// T19: Installer guard tests
func TestConfigValidateAcceptWitnessGenesis(t *testing.T) {
	t.Parallel()

	t.Run("valid with install-rekor", func(t *testing.T) {
		cfg := Config{
			InstallRekor:         true,
			AcceptWitnessGenesis: "42:" + strings.Repeat("ab", 32),
			ArgoNamespace:        "oberth-argo",
			Namespace:            "oberth",
		}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("valid config rejected: %v", err)
		}
	})

	t.Run("rejected without install-rekor", func(t *testing.T) {
		cfg := Config{
			AcceptWitnessGenesis: "42:" + strings.Repeat("ab", 32),
			ArgoNamespace:        "oberth-argo",
			Namespace:            "oberth",
		}
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "requires --install-rekor") {
			t.Fatalf("error = %v, want requires --install-rekor", err)
		}
	})

	t.Run("rejected bad format", func(t *testing.T) {
		cfg := Config{
			InstallRekor:         true,
			AcceptWitnessGenesis: "bad-format",
			ArgoNamespace:        "oberth-argo",
			Namespace:            "oberth",
		}
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "<auditID>:<sha256hex>") {
			t.Fatalf("error = %v, want format rejection", err)
		}
	})
}

func TestOberthHelmArgsWithWitnessGenesis(t *testing.T) {
	t.Parallel()
	const publicKeyPEM = "-----BEGIN PUBLIC KEY-----\nQUJD\n-----END PUBLIC KEY-----\n"
	genesisValue := "42:" + strings.Repeat("ab", 32)

	t.Run("with matching flag", func(t *testing.T) {
		cfg := Config{
			Namespace:            "oberth",
			ChartVersion:         "v0.10.54",
			InstallRekor:         true,
			AcceptWitnessGenesis: genesisValue,
		}
		rekor := RekorResult{
			ServiceAddress:  "http://rekor-server.rekor.svc:80",
			LogPublicKeyPEM: publicKeyPEM,
		}
		args := OberthHelmArgs(cfg, OpenBaoResult{}, rekor)
		found := false
		for i, arg := range args {
			if arg == "--set-string" && i+1 < len(args) && args[i+1] == "auditAnchor.acceptWitnessGenesis="+genesisValue {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("args missing --set-string auditAnchor.acceptWitnessGenesis=%s:\n%v", genesisValue, args)
		}
	})

	t.Run("without flag emits empty pin", func(t *testing.T) {
		cfg := Config{
			Namespace:    "oberth",
			ChartVersion: "v0.10.54",
			InstallRekor: true,
		}
		rekor := RekorResult{
			ServiceAddress:  "http://rekor-server.rekor.svc:80",
			LogPublicKeyPEM: publicKeyPEM,
		}
		args := OberthHelmArgs(cfg, OpenBaoResult{}, rekor)
		found := false
		for i, arg := range args {
			if arg == "--set-string" && i+1 < len(args) && args[i+1] == "auditAnchor.acceptWitnessGenesis=" {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("args missing empty pin --set-string auditAnchor.acceptWitnessGenesis=:\n%v", args)
		}
	})
}
