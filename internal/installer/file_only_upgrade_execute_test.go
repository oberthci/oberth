package installer

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFileOnlyUpgradeCompletesRealExecute(t *testing.T) {
	t.Setenv(baoTokenEnvVar, "")
	t.Setenv(vaultTokenEnvVar, "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const token = "s.file-only-upgrade-fixture"
	bootstrapCredentialInventory(t, []byte(token))
	path := filepath.Join(t.TempDir(), "recovery")
	original := []byte("OPENBAO_ROOT_TOKEN=" + token + "\n")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	host := newCertificateResumeHost(t, nil)
	cfg := Config{ContextName: "chosen", ChartPath: testLocalChart, ImageRef: fakeChartImageRef, Upgrade: true, SecretStoreUndecided: true, SecretsFile: path, ClientHost: "127.0.0.1", ClientHTTPSPort: host.port, ClientAccess: "none", Timeout: time.Second}
	if err := Execute(t.Context(), cfg, host.deps); err != nil {
		t.Fatal("file-only upgrade did not finish", err, host.output.String())
	}
	if !host.applied || len(host.runner.calls) == 0 {
		t.Fatal("upgrade did not inspect PKI and reconcile the release")
	}
	for _, call := range host.runner.calls {
		if !strings.HasPrefix(call.stdin, token+"\n") {
			t.Fatal("PKI request did not use its private file token")
		}
	}
	if !bytes.Equal(mustRead(t, path), original) || strings.Contains(host.output.String(), token) {
		t.Fatal("upgrade changed or disclosed the recovery input")
	}
}
