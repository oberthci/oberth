package installer

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNetworkPolicyProbeReportsStartAndHeartbeat(t *testing.T) {
	var output bytes.Buffer
	ticks := make(chan time.Time)
	calls := 0
	deps := Deps{Output: &output, ProbeNetworkPolicy: func(context.Context, Config, Deps) NetworkPolicyProbeResult {
		calls++
		ticks <- time.Now()
		ticks <- time.Now()
		return NetworkPolicyProbeResult{Status: "enforced", Evidence: "owned cleanup complete"}
	}}
	got := measureNetworkPolicyProgress(t.Context(), Config{}, deps, ticks)
	if calls != 1 || got.Status != "enforced" || strings.Count(output.String(), "Checking NetworkPolicy enforcement") != 1 || strings.Count(output.String(), "Still checking NetworkPolicy enforcement and cleanup") != 2 {
		t.Fatal(got, output.String())
	}
}

func TestInstallReusesEmptySecretsOutputWithSeparateRecoveryInput(t *testing.T) {
	bootstrapCredentialInventory(t, []byte("s.retry-fixture"))
	root := t.TempDir()
	input, output := filepath.Join(root, "input"), filepath.Join(root, "output")
	raw := []byte("OPENBAO_ROOT_TOKEN=s.retry-fixture\n")
	if err := os.WriteFile(input, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(output, nil, 0600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{InstallSecretStore: true, ResumeSecretsFile: input, SecretsFile: output}
	if err := prepareInstallInputs(t.Context(), &cfg, InstallDeps{}); err != nil {
		t.Fatal(err)
	}
	defer clear(cfg.resumeRootToken)
	if cfg.secretsFileInput || cfg.ResumeSecretsFile != input {
		t.Fatal("empty output treated as recovery input")
	}
	err := executeWithCredentials(t.Context(), cfg, noClusterInstallDeps(t, false).withDefaults())
	if err == nil || !strings.Contains(err.Error(), "stop: cluster reached") {
		t.Fatal("retry refused empty output before cluster access", err)
	}
	if err := os.WriteFile(output, nil, 0600); err != nil {
		t.Fatal(err)
	}
	sink, err := openSecretsFile(output, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := openSecretsFile(output, true); err == nil {
		t.Fatal("concurrent reservation accepted")
	}
	sink.add("Root token", "s.retry-fixture")
	sink.finish(io.Discard)
	info, err := os.Stat(output)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info, err)
	}
	if got, err := os.ReadFile(input); err != nil || !bytes.Equal(got, raw) {
		t.Fatal("recovery input changed")
	}
	if err := prepareInstallInputs(t.Context(), &cfg, InstallDeps{}); err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatal("non-empty conflicting output accepted", err)
	}
	if _, err := openSecretsFile(output, true); err == nil {
		t.Fatal("non-empty output overwritten")
	}
}

func TestInstallEmptySecretsOutputStillRefusesLinksAndCheckout(t *testing.T) {
	root := t.TempDir()
	empty := filepath.Join(root, "empty")
	if err := os.WriteFile(empty, nil, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(empty, link); err != nil {
		t.Fatal(err)
	}
	hard := filepath.Join(root, "hard")
	if err := os.Link(empty, hard); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".git"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(repo, "empty")
	if err := os.WriteFile(inside, nil, 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{link, hard, inside} {
		if sink, err := openSecretsFile(path, true); err == nil {
			sink.finish(io.Discard)
			t.Fatal("unsafe reusable path accepted", path)
		}
	}
}

func TestInstallRefusesReadableEmptySecretsOutput(t *testing.T) {
	for _, mode := range []os.FileMode{0644, 0640, 0604, 0660} {
		t.Run(mode.String(), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "empty")
			if err := os.WriteFile(path, nil, mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, mode); err != nil {
				t.Fatal(err)
			}
			observer, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = observer.Close() }()
			if sink, err := openSecretsFile(path, true); err == nil {
				sink.finish(io.Discard)
				t.Fatal("readable empty output accepted")
			}
			info, err := observer.Stat()
			if err != nil || info.Size() != 0 || info.Mode().Perm() != mode {
				t.Fatal("unsafe output changed instead of refusing", info, err)
			}
		})
	}
}
