package installer

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnsealRealAgeRoundTrip(t *testing.T) {
	for _, tool := range []string{"age", "age-keygen"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip("age integration tools unavailable")
		}
	}
	cfg, key := unsealFixture(t)
	defer clear(key)
	identity := filepath.Join(t.TempDir(), "identity")
	if _, err := DefaultRunCommand(t.Context(), nil, "age-keygen", "-o", identity); err != nil {
		t.Fatal("cannot generate age test identity")
	}
	recipient, err := DefaultRunCommand(t.Context(), nil, "age-keygen", "-y", identity)
	if err != nil {
		t.Fatal("cannot derive age recipient")
	}
	plain := append([]byte("OPENBAO_UNSEAL_KEY="), key...)
	defer clear(plain)
	encrypted, err := DefaultRunCommand(t.Context(), plain, "age", "--encrypt", "--recipient", strings.TrimSpace(string(recipient)))
	if err != nil {
		t.Fatal("cannot encrypt test ceremony")
	}
	if err := os.WriteFile(cfg.SecretsFile, encrypted, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadUnsealSecret(t.Context(), cfg.SecretsFile, identity, DefaultRunCommand)
	defer clear(got)
	if err != nil || !bytes.Equal(got, key) {
		t.Fatal("age round trip did not recover the unseal key")
	}
}
