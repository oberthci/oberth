package releaseverify

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"testing/fstest"
)

func signingKey(t *testing.T) (*ecdsa.PrivateKey, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return key, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}
func signed(t *testing.T, key *ecdsa.PrivateKey, body []byte) []byte {
	t.Helper()
	sum := sha256.Sum256(body)
	signature, err := ecdsa.SignASN1(rand.Reader, key, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	result, err := json.Marshal(map[string]any{
		"mediaType":        "application/vnd.dev.sigstore.bundle.v0.3+json",
		"messageSignature": map[string]any{"messageDigest": map[string]any{"algorithm": "SHA2_256", "digest": sum[:]}, "signature": signature},
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func fixture(t *testing.T, key *ecdsa.PrivateKey, version string) fstest.MapFS {
	t.Helper()
	body := []byte("shipped bytes")
	record, err := json.Marshal(Record{Version: version, Commit: strings.Repeat("a", 40), Images: map[string]string{"oberth": "sha256:" + strings.Repeat("b", 64), "oberth-source-plugin": "sha256:" + strings.Repeat("c", 64)}, Base: "sha256:" + strings.Repeat("d", 64)})
	if err != nil {
		t.Fatal(err)
	}
	sums := []byte(fmt.Sprintf("%x  oberth\n%x  release.json\n", sha256.Sum256(body), sha256.Sum256(record)))
	return fstest.MapFS{"oberth": {Data: body}, "release.json": {Data: record}, "SHA256SUMS": {Data: sums}, "SHA256SUMS.sigstore.json": {Data: signed(t, key, sums)}}
}

func TestVerifyRefusesTampered(t *testing.T) {
	key, pub := signingKey(t)
	_, other := signingKey(t)
	for _, tc := range []struct {
		name   string
		change func(fstest.MapFS)
		key    []byte
		want   error
	}{
		{"wrong key", func(fstest.MapFS) {}, other, Signature},
		{"checksum list", func(f fstest.MapFS) { f["SHA256SUMS"].Data[0] ^= 1 }, pub, Signature},
		{"missing bundle", func(f fstest.MapFS) { delete(f, "SHA256SUMS.sigstore.json") }, pub, Missing},
		{"signed downgrade", func(f fstest.MapFS) {
			for n, b := range fixture(t, key, "v1.0.0") {
				f[n] = b
			}
		}, pub, Version},
		{"invalid pin", func(fstest.MapFS) {}, []byte("invalid"), Key},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := fixture(t, key, "v2.0.0")
			tc.change(f)
			_, err := verify(context.Background(), f, tc.key, "v2.0.0")
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v; want %v", err, tc.want)
			}
		})
	}
	f := fixture(t, key, "v2.0.0")
	if _, err := verify(context.Background(), f, pub, "v2.0.0"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := verify(ctx, f, pub, "v2.0.0"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
}

func TestMetadataLimit(t *testing.T) {
	for _, n := range []int{MaxMetadata, MaxMetadata + 1} {
		_, err := readMetadata(context.Background(), fstest.MapFS{"file": {Data: bytes.Repeat([]byte("x"), n)}}, "file")
		if (err == nil) != (n == MaxMetadata) {
			t.Fatalf("size %d: %v", n, err)
		}
	}
	key, pub := signingKey(t)
	body := []byte("bytes")
	bundle := signed(t, key, body)
	// JSON permits padding whitespace, so this exercises the bound on valid bundles.
	for _, n := range []int{MaxMetadata, MaxMetadata + 1} {
		padded := append(bytes.Clone(bundle), bytes.Repeat([]byte(" "), n-len(bundle))...)
		err := verifyBlob(body, padded, pub)
		if (err == nil) != (n == MaxMetadata) {
			t.Fatalf("bundle size %d: %v", n, err)
		}
	}
}

func TestVerifyAcceptsCosignBundle(t *testing.T) {
	var files = os.DirFS("testdata/cosign-v3.1.1")
	body, err := os.ReadFile("testdata/cosign-v3.1.1/blob")
	if err != nil {
		t.Fatal(err)
	}
	pub, err := os.ReadFile("testdata/cosign-v3.1.1/public.pem")
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := readMetadata(context.Background(), files, "bundle.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyBlob(body, bundle, pub); err != nil {
		t.Fatal(err)
	}
	bundle[0] ^= 1
	if verifyBlob(body, bundle, pub) == nil {
		t.Fatal("malformed bundle accepted")
	}
}

func TestMessages(t *testing.T) {
	for _, r := range []Refusal{Key, Signature, Inventory, Digest, Metadata, Missing, Limit, Read, Version} {
		if len(r.Error()) > 80 || len(r.Remedy()) > 120 || strings.Contains(r.Error(), "...") {
			t.Fatal(r)
		}
	}
}

func TestChecksumInventorySyntax(t *testing.T) {
	key, pub := signingKey(t)
	for _, raw := range []string{"", strings.Repeat("0", 64) + "  ../file\n", strings.Repeat("0", 64) + "  release.json\n" + strings.Repeat("0", 64) + "  release.json\n", strings.Repeat("F", 64) + "  release.json\n"} {
		f := fixture(t, key, "v1.0.0")
		f["SHA256SUMS"].Data = []byte(raw)
		f["SHA256SUMS.sigstore.json"].Data = signed(t, key, []byte(raw))
		if _, err := verify(context.Background(), f, pub, "v1.0.0"); !errors.Is(err, Inventory) {
			t.Fatalf("%q: %v", raw, err)
		}
	}
}
