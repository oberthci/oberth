package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/releaseverify"
)

func publicPin(t *testing.T, scalar byte) []byte {
	t.Helper()
	raw := make([]byte, 32)
	raw[31] = scalar
	key, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), raw)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}

func pinFixture(t *testing.T, script []byte) {
	t.Helper()
	t.Chdir(t.TempDir())
	for _, edit := range []struct {
		path string
		body []byte
	}{{releaseKeyPath, nil}, {fetchScriptPath, script}} {
		if err := os.MkdirAll(filepath.Dir(edit.path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(edit.path, edit.body, 0755); err != nil {
			t.Fatal(err)
		}
	}
}

func fetchPinSource(t *testing.T) []byte {
	t.Helper()
	script, err := os.ReadFile("../../" + fetchScriptPath)
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := releaseKeySnapshot(string(script), nil)
	if err != nil {
		t.Fatal(err)
	}
	return []byte(fixture)
}

func pinInput(t *testing.T, body []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "input.pem")
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func assertPin(t *testing.T, key []byte) {
	t.Helper()
	got, err := os.ReadFile(releaseKeyPath)
	if err != nil || !bytes.Equal(got, key) {
		t.Fatal("canonical pin differs", err)
	}
	info, err := os.Stat(fetchScriptPath)
	if err != nil || info.Mode().Perm() != 0755 {
		t.Fatal("fetcher mode changed", err)
	}
}

func TestReleasePinProvisionReplaceAndRepair(t *testing.T) {
	script := fetchPinSource(t)
	pinFixture(t, script)
	first, second := publicPin(t, 1), publicPin(t, 2)
	input := pinInput(t, first)
	if err := run(t.Context(), []string{"pin", "--public-key-file", input}); err != nil {
		t.Fatal(err)
	}
	assertPin(t, first)
	before, _ := os.ReadFile(fetchScriptPath)
	if err := provisionReleaseKey(input, false); err != nil {
		t.Fatal("same key refused", err)
	}
	after, _ := os.ReadFile(fetchScriptPath)
	if !bytes.Equal(before, after) {
		t.Fatal("repeated provisioning changed the fetcher")
	}
	other := pinInput(t, second)
	if err := provisionReleaseKey(other, false); !errors.Is(err, keyReplace) {
		t.Fatal("different key replaced without consent", err)
	}
	assertPin(t, first)
	if err := provisionReleaseKey(other, true); err != nil {
		t.Fatal(err)
	}
	assertPin(t, second)
	if err := os.WriteFile(fetchScriptPath, script, 0755); err != nil {
		t.Fatal(err)
	}
	if err := provisionReleaseKey(other, false); err != nil {
		t.Fatal("same-key repair refused", err)
	}
	assertPin(t, second)
	repaired, _ := os.ReadFile(fetchScriptPath)
	if bytes.Equal(repaired, script) {
		t.Fatal("rerun left the stale fetcher untouched")
	}
}

func TestReleasePinTruncatedExistingPinIsInvalidAndCanBeReplaced(t *testing.T) {
	script := fetchPinSource(t)
	pinFixture(t, script)
	key := publicPin(t, 1)
	truncated := key[:len(key)/2]
	if err := os.WriteFile(releaseKeyPath, truncated, 0644); err != nil {
		t.Fatal(err)
	}
	input := pinInput(t, key)
	err := provisionReleaseKey(input, false)
	if !errors.Is(err, keyPinInvalid) || err.Error() != "Existing release public key pin is invalid." {
		t.Fatal("corrupt pin did not have its own refusal", err)
	}
	if keyPinInvalid.Remedy() != keyReplace.Remedy() {
		t.Fatal("corrupt pin lost the replacement remedy")
	}
	assertPin(t, truncated)
	unchanged, err := os.ReadFile(fetchScriptPath)
	if err != nil || !bytes.Equal(unchanged, script) {
		t.Fatal("refused pin changed the fetcher", err)
	}
	if err := provisionReleaseKey(input, true); err != nil {
		t.Fatal("explicit replacement did not repair the corrupt pin", err)
	}
	assertPin(t, key)
}

func TestReleasePinRefusesInvalidInputsWithoutWrites(t *testing.T) {
	script := fetchPinSource(t)
	for _, body := range [][]byte{[]byte("not a public key"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("private fixture")})} {
		t.Run("invalid", func(t *testing.T) {
			pinFixture(t, script)
			if err := provisionReleaseKey(pinInput(t, body), true); !errors.Is(err, keyInvalid) {
				t.Fatal("invalid key accepted with replacement", err)
			}
			assertPin(t, nil)
		})
	}
}

func TestReleasePinRefusesMalformedFetcherWithoutWrites(t *testing.T) {
	script := string(fetchPinSource(t))
	for name, change := range map[string]string{
		"missing block":  strings.ReplaceAll(script, keyBlockStart, ""),
		"repeated block": script + keyBlockStart,
		"missing end":    strings.ReplaceAll(script, keyBlockEnd, ""),
		"repeated end":   script + keyBlockEnd,
	} {
		t.Run(name, func(t *testing.T) {
			pinFixture(t, []byte(change))
			if err := provisionReleaseKey(pinInput(t, publicPin(t, 1)), false); !errors.Is(err, keyMarkers) {
				t.Fatal("malformed fetcher accepted", err)
			}
			key, _ := os.ReadFile(releaseKeyPath)
			got, _ := os.ReadFile(fetchScriptPath)
			if len(key) != 0 || string(got) != change {
				t.Fatal("failed preflight changed a consumer")
			}
		})
	}
}

func TestReleasePinWriteFailureLeavesCompletedFirstWrite(t *testing.T) {
	script := fetchPinSource(t)
	pinFixture(t, script)
	key := publicPin(t, 1)
	if err := writePinFile(releaseKeyPath, nil, key); err != nil {
		t.Fatal(err)
	}
	if err := writePinFile("missing/path", nil, []byte("unwritable")); err == nil {
		t.Fatal("second write unexpectedly succeeded")
	}
	assertPin(t, key)
	unchanged, err := os.ReadFile(fetchScriptPath)
	if err != nil || !bytes.Equal(unchanged, script) {
		t.Fatal("failed second write changed the old fetcher", err)
	}
}

type keyReadCounter struct {
	io.Reader
	bytes int
}

func (r *keyReadCounter) Read(body []byte) (int, error) {
	n, err := r.Reader.Read(body)
	r.bytes += n
	return n, err
}

func TestReleasePinReadUsesMetadataBound(t *testing.T) {
	reader := &keyReadCounter{Reader: strings.NewReader(strings.Repeat(" ", 2*releaseverify.MaxMetadata))}
	if _, err := readKeyBytes(reader); !errors.Is(err, keyInvalid) {
		t.Fatal("oversize key read was accepted", err)
	}
	if reader.bytes != releaseverify.MaxMetadata+1 {
		t.Fatal("key reader did not stop at the existing bound plus one", reader.bytes)
	}
	public := publicPin(t, 1)
	for _, extra := range []int{0, 1} {
		body := append(bytes.Clone(public), bytes.Repeat([]byte(" "), releaseverify.MaxMetadata-len(public)+extra)...)
		got, err := readPublicKey(pinInput(t, body))
		if extra == 0 && (err != nil || !bytes.Equal(got, public)) {
			t.Fatal("exact-bound public key refused", err)
		}
		if extra != 0 && !errors.Is(err, keyInvalid) {
			t.Fatal("bound-plus-one public key accepted", err)
		}
	}
}
