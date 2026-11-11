package installer

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func unsealFixture(t *testing.T) (UnsealConfig, []byte) {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	key = []byte(base64.StdEncoding.EncodeToString(key))
	file := filepath.Join(t.TempDir(), "secrets")
	content := append([]byte("# installer ceremony\nOPENBAO_UNSEAL_KEY="), key...)
	content = append(content, '\n')
	if err := os.WriteFile(file, content, 0o600); err != nil {
		t.Fatal(err)
	}
	clear(content)
	return UnsealConfig{SecretsFile: file, Context: "chosen", Namespace: "openbao", Pod: "openbao-0"}, key
}

func TestUnsealKeepsCredentialsOnStdin(t *testing.T) {
	for _, failure := range []string{"", "remote echo", "invalid JSON", "still sealed", "missing status"} {
		t.Run(failure, func(t *testing.T) {
			cfg, key := unsealFixture(t)
			defer clear(key)
			calls := 0
			run := func(_ context.Context, input []byte, name string, args ...string) ([]byte, error) {
				calls++
				if name != "kubectl" || bytes.Contains([]byte(strings.Join(args, " ")), key) {
					t.Fatal("unsafe subprocess arguments")
				}
				if !strings.Contains(strings.Join(args, " "), "--context chosen") {
					t.Fatal("lost user-selected context")
				}
				if calls == 1 {
					return []byte(`{"initialized":true,"sealed":true}`), errors.New("exit 2")
				}
				var body struct {
					Key string `json:"key"`
				}
				if json.Unmarshal(input, &body) != nil || body.Key != string(key) {
					t.Fatal("key did not travel as JSON stdin")
				}
				if failure == "remote echo" {
					return bytes.Clone(input), errors.New(string(input))
				}
				if failure == "invalid JSON" {
					return bytes.Clone(key), nil
				}
				if failure == "still sealed" {
					return []byte(`{"sealed":true}`), nil
				}
				if failure == "missing status" {
					return []byte(`{}`), nil
				}
				return []byte(`{"sealed":false}`), nil
			}
			sealed, err := Unseal(t.Context(), cfg, run)
			if failure == "" && (err != nil || sealed) {
				t.Fatal("expected successful unseal")
			}
			if failure != "" && err == nil {
				t.Fatal("expected refusal")
			}
			if err != nil && strings.Contains(err.Error(), string(key)) {
				t.Fatal("credential leaked through error")
			}
			if calls != 2 {
				t.Fatal("unexpected call count")
			}
		})
	}
}

func TestUnsealAlreadyReadyDoesNotReadSecrets(t *testing.T) {
	cfg := UnsealConfig{SecretsFile: "/nonexistent", Context: "chosen", Namespace: "openbao", Pod: "openbao-0"}
	sealed, err := Unseal(t.Context(), cfg, func(context.Context, []byte, string, ...string) ([]byte, error) {
		return []byte(`{"initialized":true,"sealed":false}`), nil
	})
	if err != nil || sealed {
		t.Fatal("ready store should not require credentials")
	}
}

func TestUnsealReadsAgeWithoutDisclosingErrors(t *testing.T) {
	cfg, key := unsealFixture(t)
	defer clear(key)
	if err := os.WriteFile(cfg.SecretsFile, []byte("age-encryption.org/v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, fail := range []bool{false, true} {
		run := func(_ context.Context, input []byte, name string, args ...string) ([]byte, error) {
			if name != "age" || strings.Join(args, " ") != "--decrypt --identity /private/identity" {
				t.Fatal("unexpected age invocation")
			}
			if !bytes.HasPrefix(input, []byte("age-encryption.org/v1")) {
				t.Fatal("ciphertext did not travel on stdin")
			}
			if fail {
				return bytes.Clone(key), errors.New(string(key))
			}
			return append([]byte("OPENBAO_UNSEAL_KEY="), key...), nil
		}
		got, err := ReadUnsealSecret(t.Context(), cfg.SecretsFile, "/private/identity", run)
		if fail {
			if err == nil || strings.Contains(err.Error(), string(key)) {
				t.Fatal("unsafe age failure")
			}
		} else if err != nil || !bytes.Equal(got, key) {
			t.Fatal("age plaintext not parsed")
		}
		clear(got)
	}
	if _, err := ReadUnsealSecret(t.Context(), cfg.SecretsFile, "", nil); err == nil {
		t.Fatal("encrypted file needs an identity")
	}
}

func TestUnsealRefusesUnsafeFilesAndMalformedCeremonies(t *testing.T) {
	cfg, key := unsealFixture(t)
	defer clear(key)
	for _, contents := range []string{"", "OPENBAO_UNSEAL_KEY=\n", "OPENBAO_UNSEAL_KEY=a\nOPENBAO_UNSEAL_KEY=b\n", "OPENBAO_UNSEAL_KEY=a b\n", "$(do-not-execute)\n", "OPENBAO_ROOT_TOKEN=value\n"} {
		if err := os.WriteFile(cfg.SecretsFile, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadUnsealSecret(t.Context(), cfg.SecretsFile, "", nil); err == nil {
			t.Fatal("accepted malformed ceremony")
		}
	}
	if err := os.Chmod(cfg.SecretsFile, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadUnsealSecret(t.Context(), cfg.SecretsFile, "", nil); err == nil {
		t.Fatal("accepted public credential file")
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(cfg.SecretsFile, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadUnsealSecret(t.Context(), link, "", nil); err == nil {
		t.Fatal("accepted symlink")
	}
	if _, err := ReadUnsealSecret(t.Context(), t.TempDir(), "", nil); err == nil {
		t.Fatal("accepted directory")
	}
}

func TestTPMUnsealRefusesUnsupportedHosts(t *testing.T) {
	for _, failure := range []string{"no TPM", "multi-node", "managed", "different host", "encrypt", "", "k3s"} {
		t.Run(failure, func(t *testing.T) {
			cfg, key := unsealFixture(t)
			defer clear(key)
			dest := filepath.Join(t.TempDir(), "unseal.cred")
			run := func(_ context.Context, input []byte, name string, args ...string) ([]byte, error) {
				joined := strings.Join(args, " ")
				if bytes.Contains([]byte(joined), key) {
					t.Fatal("key in argv")
				}
				switch name {
				case "kubectl":
					nodes := `{"items":[{"status":{"nodeInfo":{"machineID":"this-host"}}}]}`
					if failure == "multi-node" {
						nodes = `{"items":[{},{}]}`
					}
					if failure == "k3s" {
						nodes = `{"items":[{"spec":{"providerID":"k3s://node"},"status":{"nodeInfo":{"machineID":"this-host"}}}]}`
					}
					if failure == "managed" {
						nodes = `{"items":[{"spec":{"providerID":"cloud://node"},"status":{"nodeInfo":{"machineID":"this-host"}}}]}`
					}
					if failure == "different host" {
						nodes = `{"items":[{}]}`
					}
					return []byte(nodes), nil
				case "systemd-creds":
					if joined == "has-tpm2" {
						if failure == "no TPM" {
							return bytes.Clone(key), errors.New(string(key))
						}
						return nil, nil
					}
					if !strings.Contains(joined, "--with-key=tpm2 --tpm2-device=auto --name=oberth-secrets") || !bytes.Contains(input, key) {
						t.Fatal("missing TPM binding or stdin credential")
					}
					if failure == "encrypt" {
						return bytes.Clone(key), errors.New(string(key))
					}
					return []byte("opaque encrypted credential"), nil
				}
				t.Fatal("unexpected executable")
				return nil, nil
			}
			err := sealUnsealCredential(t.Context(), cfg, dest, []byte("this-host\n"), run)
			if failure != "" && failure != "k3s" {
				if err == nil || strings.Contains(err.Error(), string(key)) {
					t.Fatal("unsafe TPM failure")
				}
				if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("refusal wrote a credential")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				data, err := os.ReadFile(dest)
				if err != nil || bytes.Contains(data, key) {
					t.Fatal("plaintext written")
				}
				info, err := os.Stat(dest)
				if err != nil || info.Mode().Perm() != 0o600 {
					t.Fatal("unsafe permissions")
				}
				if err := sealUnsealCredential(t.Context(), cfg, dest, []byte("this-host"), run); err == nil {
					t.Fatal("overwrote credential")
				}
			}
		})
	}
}

// Exercise the real process boundary as well as the injected runner: argv,
// inherited environment and captured diagnostics must not disclose the key.
func TestUnsealSubprocessDoesNotExposeCredentials(t *testing.T) {
	cfg, key := unsealFixture(t)
	defer clear(key)
	bin := t.TempDir()
	trace := filepath.Join(t.TempDir(), "public-process-state")
	script := `#!/bin/sh
printf '%s\n' "$@" > "$UNSEAL_TEST_TRACE"
env >> "$UNSEAL_TEST_TRACE"
case " $* " in
 *" status "*) printf '%s\n' '{"initialized":true,"sealed":true}'; exit 2 ;;
 *) cat >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "kubectl"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("UNSEAL_TEST_TRACE", trace)
	_, err := Unseal(t.Context(), cfg, nil)
	if err == nil || strings.Contains(err.Error(), string(key)) {
		t.Fatal("unsafe subprocess failure")
	}
	state, readErr := os.ReadFile(trace)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if bytes.Contains(state, key) {
		t.Fatal("secret appeared in argv or process environment")
	}
}
