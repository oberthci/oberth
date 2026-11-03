package installer

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
)

func TestSecretStoreChoiceKeepsExistingInstall(t *testing.T) {
	for _, interactive := range []bool{false, true} {
		var output bytes.Buffer
		cfg := Config{Namespace: "oberth", SecretStoreUndecided: true}
		deps := Deps{Output: &output, Input: strings.NewReader("must not be read"), IsTerminal: func() bool { return interactive },
			RunHelm: func(_ context.Context, args []string) ([]byte, error) {
				switch strings.Join(args[:2], " ") {
				case "list -n":
					return []byte(`[{"name":"oberth","status":"deployed"}]`), nil
				case "get values":
					return []byte(`{"secretstore":{"address":"https://bao.example"},"tls":{"pki":{"enabled":true}}}`), nil
				default:
					t.Fatalf("unexpected command: %v", args)
					return nil, nil
				}
			},
		}
		if err := promptSecretStoreChoice(t.Context(), &cfg, deps); err != nil {
			t.Fatal(err)
		}
		if cfg.InstallSecretStore || cfg.InstallSecretStoreDev || !cfg.serverPKI {
			t.Fatalf("existing store changed: install=%v dev=%v pki=%v", cfg.InstallSecretStore, cfg.InstallSecretStoreDev, cfg.serverPKI)
		}
		if strings.Contains(output.String(), "Choice") {
			t.Fatal("existing install prompted")
		}
	}
}

func TestSecretStoreChoiceFailsOnHelmReadError(t *testing.T) {
	var output bytes.Buffer
	broken := errors.New("helm unavailable")
	deps := Deps{Output: &output, RunHelm: func(context.Context, []string) ([]byte, error) { return nil, broken }}
	if err := promptSecretStoreChoice(t.Context(), &Config{}, deps); !errors.Is(err, broken) {
		t.Fatalf("error=%v", err)
	}
}

func TestSecretStoreChoiceKeepsAnAlreadyResolvedMode(t *testing.T) {
	// --install-rekor resolves production OpenBao during Config.Validate even
	// when neither explicit secret-store flag was supplied at the CLI.
	cfg := Config{InstallRekor: true, SecretStoreUndecided: true}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := promptSecretStoreChoice(t.Context(), &cfg, Deps{Output: &output}); err != nil {
		t.Fatal(err)
	}
	if !cfg.InstallSecretStore || output.Len() != 0 {
		t.Fatal("resolved production mode was changed or prompted")
	}
}

func TestFreshNonInteractiveInstallRefusesBeforeAnyWrite(t *testing.T) {
	client := fake.NewClientset()
	var output bytes.Buffer
	deps := Deps{Output: &output, KubeClient: client, RestConfig: &rest.Config{Host: "https://127.0.0.1:6443"},
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			if args[0] != "list" {
				t.Fatalf("fresh install continued past secret-store selection: %v", args)
			}
			return []byte(`[]`), nil
		},
		RunCommand: func(context.Context, []byte, string, ...string) ([]byte, error) {
			t.Fatal("fresh undecided install must not execute commands")
			return nil, nil
		},
	}
	err := Run(t.Context(), Config{SecretStoreUndecided: true}, deps)
	if err == nil || !strings.Contains(err.Error(), "--install-secretstore") {
		t.Fatalf("missing explicit OpenBao selection did not stop installation: %v", err)
	}
	for _, action := range client.Actions() {
		if action.GetVerb() != "get" && action.GetVerb() != "list" {
			t.Fatalf("refused installation mutated Kubernetes: %s %s", action.GetVerb(), action.GetResource())
		}
	}
}
