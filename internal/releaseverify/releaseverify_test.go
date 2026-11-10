package releaseverify

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The compiled-in key is the release pipeline's pin, byte for byte.
func TestPinnedKeyIsTheReleasePipelinePin(t *testing.T) {
	t.Parallel()
	pin, err := os.ReadFile(filepath.Join("..", "..", ".oberth", "pins", "release-cosign.pub"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pin, ReleasePublicKeyPEM) {
		t.Fatal("internal/releaseverify/release-cosign.pub differs from .oberth/pins/release-cosign.pub")
	}
	script, err := os.ReadFile(filepath.Join("..", "..", ".oberth", "release.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(script, []byte(ReleasePublicKeySHA256)) {
		t.Fatal("release.sh pins a different release key digest")
	}
	if _, err := PinnedKey(); err != nil {
		t.Fatal(err)
	}
}

// A real published record and the bundle cosign wrote for it verify.
func TestVerifyBlobPublishedRelease(t *testing.T) {
	t.Parallel()
	record, err := os.ReadFile(filepath.Join("testdata", "v0.20.1-release.json"))
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := os.ReadFile(filepath.Join("testdata", "v0.20.1-release.json.sigstore.json"))
	if err != nil {
		t.Fatal(err)
	}
	key, err := PinnedKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyBlob(record, bundle, key); err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseRecord(record, "v0.20.1")
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Chart.Version != "0.20.1" || !strings.Contains(parsed.Images.Server.Ref, "@sha256:") {
		t.Fatalf("record = %+v", parsed)
	}
	tampered := bytes.Replace(record, []byte(`"0.20.1"`), []byte(`"0.20.2"`), 1)
	if err := VerifyBlob(tampered, bundle, key); err == nil {
		t.Fatal("a changed record verified")
	}
	other := testKey(t)
	if err := VerifyBlob(record, bundle, &other.PublicKey); err == nil {
		t.Fatal("record verified under another key")
	}
	if _, err := ParseRecord(record, "v0.20.2"); err == nil {
		t.Fatal("record for v0.20.1 accepted as v0.20.2")
	}
}

func testKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// SignBundleForTest produces a bundle of the shape cosign sign-blob writes.
func signBundle(t *testing.T, key *ecdsa.PrivateKey, payload []byte) []byte {
	t.Helper()
	sum := sha256.Sum256(payload)
	signature, err := ecdsa.SignASN1(rand.Reader, key, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	hint := sha256.Sum256(der)
	raw, err := json.Marshal(map[string]any{
		"mediaType":            "application/vnd.dev.sigstore.bundle.v0.3+json",
		"verificationMaterial": map[string]any{"publicKey": map[string]any{"hint": base64.StdEncoding.EncodeToString(hint[:])}},
		"messageSignature": map[string]any{
			"messageDigest": map[string]any{"algorithm": "SHA2_256", "digest": base64.StdEncoding.EncodeToString(sum[:])},
			"signature":     base64.StdEncoding.EncodeToString(signature),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func testRelease(t *testing.T, key *ecdsa.PrivateKey, tag string, chart []byte) Files {
	t.Helper()
	sum := sha256.Sum256(chart)
	record, err := json.Marshal(map[string]any{
		"schemaVersion": 1, "component": "oberth", "version": tag,
		"source": map[string]any{"repository": SourceRepository, "sha": strings.Repeat("a", 40)},
		"images": map[string]any{"server": map[string]any{"ref": "registry.example/oberth/oberth@sha256:" + strings.Repeat("b", 64)}},
		"chart":  map[string]any{"version": strings.TrimPrefix(tag, "v"), "digest": "sha256:" + hex.EncodeToString(sum[:])},
	})
	if err != nil {
		t.Fatal(err)
	}
	return Files{Record: record, RecordBundle: signBundle(t, key, record), Chart: chart, ChartBundle: signBundle(t, key, chart)}
}

func TestVerifyReleaseFailsClosed(t *testing.T) {
	t.Parallel()
	key := testKey(t)
	chart := []byte("chart bytes")
	files := testRelease(t, key, "v1.2.3", chart)
	release, err := Verify(files, "v1.2.3", &key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(release.Chart, chart) {
		t.Fatal("verified chart differs")
	}
	other := testKey(t)
	for name, mutate := range map[string]func(*Files){
		"chart swapped":       func(f *Files) { f.Chart = []byte("other chart"); f.ChartBundle = signBundle(t, key, f.Chart) },
		"chart unsigned":      func(f *Files) { f.ChartBundle = nil },
		"chart foreign key":   func(f *Files) { f.ChartBundle = signBundle(t, other, f.Chart) },
		"record foreign key":  func(f *Files) { f.RecordBundle = signBundle(t, other, f.Record) },
		"record wrong bundle": func(f *Files) { f.RecordBundle = f.ChartBundle },
		"record wrong tag": func(f *Files) {
			*f = testRelease(t, key, "v1.2.4", chart)
		},
	} {
		changed := files
		mutate(&changed)
		if _, err := Verify(changed, "v1.2.3", &key.PublicKey); err == nil {
			t.Errorf("%s: verified", name)
		}
	}
}

func TestParseRecordRefusesUnpinned(t *testing.T) {
	t.Parallel()
	base := map[string]any{
		"schemaVersion": 1, "component": "oberth", "version": "v1.2.3",
		"source": map[string]any{"repository": SourceRepository, "sha": strings.Repeat("a", 40)},
		"images": map[string]any{"server": map[string]any{"ref": "registry.example/oberth/oberth@sha256:" + strings.Repeat("b", 64)}},
		"chart":  map[string]any{"version": "1.2.3", "digest": "sha256:" + strings.Repeat("c", 64)},
	}
	for name, change := range map[string]func(map[string]any){
		"tag image": func(r map[string]any) {
			r["images"] = map[string]any{"server": map[string]any{"ref": "registry.example/oberth/oberth:v1.2.3"}}
		},
		"chart digest": func(r map[string]any) { r["chart"] = map[string]any{"version": "1.2.3", "digest": "c"} },
		"component":    func(r map[string]any) { r["component"] = "other" },
		"schema":       func(r map[string]any) { r["schemaVersion"] = 2 },
		"source": func(r map[string]any) {
			r["source"] = map[string]any{"repository": "example.com/fork", "sha": strings.Repeat("a", 40)}
		},
		"chart version": func(r map[string]any) {
			r["chart"] = map[string]any{"version": "1.2.4", "digest": "sha256:" + strings.Repeat("c", 64)}
		},
	} {
		record := map[string]any{}
		for k, v := range base {
			record[k] = v
		}
		change(record)
		raw, _ := json.Marshal(record)
		if _, err := ParseRecord(raw, "v1.2.3"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestFetchAndLoadDir(t *testing.T) {
	t.Parallel()
	key := testKey(t)
	files := testRelease(t, key, "v1.2.3", []byte("chart"))
	served := map[string][]byte{
		"https://releases.example/oberth/v1.2.3/release.json":               files.Record,
		"https://releases.example/oberth/v1.2.3/release.json.sigstore.json": files.RecordBundle,
		"https://charts.example/oberth-1.2.3.tgz":                           files.Chart,
		"https://charts.example/oberth-1.2.3.tgz.sigstore.json":             files.ChartBundle,
	}
	get := func(_ context.Context, url string, _ int64) ([]byte, error) {
		body, ok := served[url]
		if !ok {
			return nil, errors.New("404 " + url)
		}
		return body, nil
	}
	fetched, err := Fetch(context.Background(), get, "https://releases.example", "https://charts.example/", "v1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(fetched, "v1.2.3", &key.PublicKey); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for url, body := range served {
		if err := os.WriteFile(filepath.Join(dir, url[strings.LastIndex(url, "/")+1:]), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	loaded, err := LoadDir(dir, "v1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(loaded, "v1.2.3", &key.PublicKey); err != nil {
		t.Fatal(err)
	}
	if _, err := HTTPSGetter(context.Background(), "http://releases.example/x", 1); err == nil {
		t.Fatal("plain HTTP accepted")
	}
	if _, err := PublicKey(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: []byte("x")})); err == nil {
		t.Fatal("garbage key accepted")
	}
}
