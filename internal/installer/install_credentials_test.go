package installer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Both install reruns and install --upgrade traverse identity preflight before
// production-store provisioning. Exercise all token consumers, using the real
// input reader and OpenBao request builder; only the Pod transport is simulated.
func TestInstallRecoveryFileSuppliesEveryPKICheck(t *testing.T) {
	t.Setenv(baoTokenEnvVar, "")
	t.Setenv("VAULT_TOKEN", "")
	const token = "s.install-file-only-credential"
	bootstrapCredentialInventory(t, []byte(token))
	for _, mode := range []struct {
		name           string
		upgrade, store bool
	}{
		{"resume", false, true}, {"upgrade_with_store", true, true}, {"upgrade_existing_PKI", true, false},
	} {
		t.Run(mode.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "credentials")
			original := []byte("OPENBAO_ROOT_TOKEN=" + token + "\n")
			if err := os.WriteFile(path, original, 0600); err != nil {
				t.Fatal(err)
			}
			cfg := Config{InstallSecretStore: mode.store, Upgrade: mode.upgrade, serverPKI: true, TLSExtraDNSNames: []string{"ci.example.com"}, TLSExtraIPs: []string{"192.0.2.1"}}
			if mode.upgrade {
				cfg.SecretsFile = path
			} else {
				cfg.ResumeSecretsFile = path
			}
			var output bytes.Buffer
			if err := prepareInstallInputs(t.Context(), &cfg, InstallDeps{Output: &output}); err != nil {
				t.Fatal(err)
			}
			defer clear(cfg.resumeRootToken)
			if cfg.InstallSecretStore != mode.store {
				t.Fatal("recovery input changed the requested store mode")
			}
			deps, runner := pkiLifecycleDeps(t, []string{"ci.example.com"}, []string{"192.0.2.1"})
			deps.Output = &output
			run := deps.RunCommand
			deps.RunCommand = func(ctx context.Context, input []byte, name string, args ...string) ([]byte, error) {
				if strings.Contains(strings.Join(args, " "), token) || os.Getenv(baoTokenEnvVar) != "" || os.Getenv(vaultTokenEnvVar) != "" {
					t.Fatal("credential reached argv or host environment")
				}
				return run(ctx, input, name, args...)
			}
			if err := cfg.Validate(); err != nil {
				t.Fatal(err)
			}
			if err := preflightServerIdentities(t.Context(), &cfg, deps, false); err != nil {
				t.Fatal(err)
			}
			_, got, err := serverPKIAccess(t.Context(), cfg, deps)
			if err != nil || got != token {
				t.Fatal("PKI access did not use recovery input")
			}
			// This is the already-initialized store setup reached later by Run.
			runner.responses["status -format=json"] = fakeBaoResponse{out: `{"initialized":true,"sealed":false}`}
			runner.responses["read -format=json oberth-pki/issuer/default"] = fakeBaoResponse{out: `{"data":{}}`}
			if cfg.InstallSecretStore {
				if _, err := setupProductionSecretStoreDeferred(t.Context(), cfg, deps, deps, OpenBaoResult{Namespace: DefaultOpenBaoNamespace}, nil); err != nil {
					t.Fatal(err)
				}
			}
			for _, call := range runner.calls {
				if call.command == "status -format=json" {
					continue
				}
				if !strings.HasPrefix(call.stdin, token+"\n") {
					t.Fatal("authenticated call did not use private file token")
				}
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(after, original) || bytes.Contains(output.Bytes(), []byte(token)) {
				t.Fatal("input changed or credential disclosed")
			}
		})
	}
}

func TestMissingRecoveryRootTokenNamesAcceptedSources(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials")
	if err := os.WriteFile(path, []byte("OBERTH_UPLINK_TOKEN=private-fixture\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{InstallSecretStore: true, SecretsFile: path, Upgrade: true}
	err := prepareInstallInputs(t.Context(), &cfg, InstallDeps{})
	if err == nil {
		t.Fatal("missing root token accepted")
	}
	for _, want := range []string{"no OPENBAO_ROOT_TOKEN", "--resume-secrets-file", "--secrets-file", "BAO_TOKEN"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "private-fixture") {
		t.Fatal("recovery value disclosed")
	}
}

func TestReusedSecretsFilePreservesInputAndReservesNewOutput(t *testing.T) {
	bootstrapCredentialInventory(t, []byte("s.recovery-input"))
	path := filepath.Join(t.TempDir(), "credentials")
	original := []byte("OPENBAO_ROOT_TOKEN=s.recovery-input\n")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{SecretsFile: path, Upgrade: true}
	if err := prepareInstallInputs(t.Context(), &cfg, InstallDeps{}); err != nil {
		t.Fatal(err)
	}
	defer clear(cfg.resumeRootToken)
	// The real Execute credential wrapper reserves its sink before contacting
	// Kubernetes. A transport error must preserve the original recovery input.
	err := executeWithCredentials(t.Context(), cfg, noClusterInstallDeps(t, false).withDefaults())
	if err == nil || !strings.Contains(err.Error(), "stop: cluster reached") {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, original) {
		t.Fatal("reused secrets file changed")
	}
	files, err := filepath.Glob(path + ".new-*")
	if err != nil || len(files) != 0 {
		t.Fatal("empty credential output was retained")
	}
}

func TestDryRunReadsRecoveryFlagsWithoutWriting(t *testing.T) {
	bootstrapCredentialInventory(t, []byte("s.dry-run-read-only"))
	for _, flag := range []string{"resume-secrets-file", "secrets-file"} {
		for _, valid := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/valid=%t", flag, valid), func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "credentials")
				raw := []byte("OPENBAO_ROOT_TOKEN=s.dry-run-read-only\n")
				mode := os.FileMode(0600)
				if !valid {
					mode = 0644
				}
				if err := os.WriteFile(path, raw, mode); err != nil {
					t.Fatal(err)
				}
				cfg := Config{DryRun: true, InstallSecretStore: true}
				if flag == "resume-secrets-file" {
					cfg.ResumeSecretsFile = path
				} else {
					cfg.SecretsFile = path
				}
				err := prepareInstallInputs(t.Context(), &cfg, InstallDeps{})
				defer clear(cfg.resumeRootToken)
				if valid && (err != nil || string(cfg.resumeRootToken) != "s.dry-run-read-only") {
					t.Fatal("recovery file not read", err)
				}
				if !valid && err == nil {
					t.Fatal("public recovery file accepted")
				}
				after, readErr := os.ReadFile(path)
				entries, dirErr := os.ReadDir(filepath.Dir(path))
				if readErr != nil || !bytes.Equal(after, raw) || dirErr != nil || len(entries) != 1 {
					t.Fatal("dry run changed recovery files")
				}
			})
		}
	}
}

func TestDryRunJSONReturnsResolvedReviewWithoutVerification(t *testing.T) {
	var output bytes.Buffer
	cfg := Config{DryRun: true}
	err := executeJSON(t.Context(), cfg, InstallDeps{Output: &output}, func(_ context.Context, got Config, deps InstallDeps) error {
		if !got.DryRun {
			t.Fatal("lost dry-run mode")
		}
		got.ContextName, got.ClientHost = "selected", "ci.example.test"
		deps.installResolved(got)
		_, _ = deps.Output.Write([]byte("private dry-run diagnostics"))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Version              int           `json:"version"`
		Review               InstallReview `json:"review"`
		ConfirmationRequired bool          `json:"confirmation_required"`
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Version != 1 || result.Review.Context != "selected" || result.Review.ClientHost != "ci.example.test" || !result.ConfirmationRequired || strings.Contains(output.String(), "private dry-run diagnostics") {
		t.Fatal("invalid public review", output.String())
	}
}

func TestDryRunJSONFailureReturnsSafeChecks(t *testing.T) {
	var output bytes.Buffer
	failure := errors.New("private dependency diagnostic")
	err := executeJSON(t.Context(), Config{DryRun: true, ContextName: "chosen"}, InstallDeps{Output: &output}, func(context.Context, Config, InstallDeps) error { return failure })
	if !errors.Is(err, failure) {
		t.Fatal("failure lost", err)
	}
	var result InstallResult
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal("no JSON on failure", err)
	}
	if result.Version != 1 || result.OK || result.Review == nil || result.Review.Context != "chosen" || len(result.Checks) != 1 || result.Checks[0].OK || result.Checks[0].Code != "dry_run" || strings.Contains(output.String(), failure.Error()) {
		t.Fatal("invalid or unsafe failure JSON", output.String())
	}
}
