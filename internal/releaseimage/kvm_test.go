package releaseimage

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

// fakeRuntimeImage creates a minimal linux/amd64 image usable as a runtime
// base, containing at least the QEMU binary path.
func fakeRuntimeImage(t *testing.T) v1.Image {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, entry := range []struct {
		name    string
		content []byte
		mode    int64
		isDir   bool
	}{
		{"usr/", nil, 0o755, true},
		{"usr/bin/", nil, 0o755, true},
		{"usr/bin/qemu-system-x86_64", []byte("fake-qemu"), 0o755, false},
		{"usr/share/", nil, 0o755, true},
		{"usr/share/qemu/", nil, 0o755, true},
		{"usr/share/qemu/bios-256k.bin", []byte("bios"), 0o644, false},
		{"etc/", nil, 0o755, true},
		{"etc/ssl/", nil, 0o755, true},
		{"etc/ssl/certs/", nil, 0o755, true},
		{"etc/ssl/certs/ca-certificates.crt", []byte("certs"), 0o644, false},
	} {
		typeFlag := byte(tar.TypeReg)
		if entry.isDir {
			typeFlag = tar.TypeDir
		}
		if err := tw.WriteHeader(&tar.Header{
			Name: entry.name, Mode: entry.mode, Size: int64(len(entry.content)),
			Typeflag: typeFlag, Format: tar.FormatPAX,
		}); err != nil {
			t.Fatal(err)
		}
		if len(entry.content) > 0 {
			if _, err := tw.Write(entry.content); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(buf.Bytes())), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	img, err := mutate.AppendLayers(empty.Image, layer)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := img.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	cfg.OS = "linux"
	cfg.Architecture = "amd64"
	img, err = mutate.ConfigFile(img, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func TestBuildHarnessImage(t *testing.T) {
	runtime := fakeRuntimeImage(t)
	binary := []byte("harness-binary-content")
	created := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

	img, err := BuildHarnessImage(runtime, binary, created, "v1.0.0", "abc123")
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := img.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Config.User != "65534:65534" {
		t.Errorf("user = %q, want 65534:65534", cfg.Config.User)
	}
	if len(cfg.Config.Entrypoint) != 1 || cfg.Config.Entrypoint[0] != "/usr/local/bin/oberth-kvm-harness" {
		t.Errorf("entrypoint = %v, want [/usr/local/bin/oberth-kvm-harness]", cfg.Config.Entrypoint)
	}
	if cfg.Architecture != "amd64" || cfg.OS != "linux" {
		t.Errorf("platform = %s/%s, want linux/amd64", cfg.OS, cfg.Architecture)
	}
	if cfg.Config.Labels["org.opencontainers.image.version"] != "v1.0.0" {
		t.Errorf("version label = %q", cfg.Config.Labels["org.opencontainers.image.version"])
	}

	// Verify the harness binary is in the last layer.
	layers, err := img.Layers()
	if err != nil {
		t.Fatal(err)
	}
	if len(layers) != 2 {
		t.Fatalf("got %d layers, want 2 (runtime + harness)", len(layers))
	}
	found := false
	reader, err := layers[1].Uncompressed()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	tr := tar.NewReader(reader)
	for {
		h, nextErr := tr.Next()
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			t.Fatal(nextErr)
		}
		if h.Name == harnessImagePath {
			if h.Mode&0o111 == 0 {
				t.Error("harness binary is not executable")
			}
			body, _ := io.ReadAll(tr)
			if !bytes.Equal(body, binary) {
				t.Error("harness binary content mismatch")
			}
			found = true
		}
	}
	if !found {
		t.Error("harness binary not found in layer")
	}
}

func TestBuildHarnessImageRejectsEmptyBinary(t *testing.T) {
	runtime := fakeRuntimeImage(t)
	if _, err := BuildHarnessImage(runtime, nil, time.Now(), "v1.0.0", "abc"); err == nil {
		t.Fatal("accepted empty binary")
	}
}

func TestBuildHarnessImageRejectsZeroTime(t *testing.T) {
	runtime := fakeRuntimeImage(t)
	if _, err := BuildHarnessImage(runtime, []byte("x"), time.Time{}, "v1.0.0", "abc"); err == nil {
		t.Fatal("accepted zero time")
	}
}

func TestBuildHarnessImageReproducible(t *testing.T) {
	runtime := fakeRuntimeImage(t)
	binary := []byte("deterministic-harness-binary")
	created := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

	img1, err := BuildHarnessImage(runtime, binary, created, "v1.0.0", "abc123")
	if err != nil {
		t.Fatal(err)
	}
	img2, err := BuildHarnessImage(runtime, binary, created, "v1.0.0", "abc123")
	if err != nil {
		t.Fatal(err)
	}

	d1, err := img1.Digest()
	if err != nil {
		t.Fatal(err)
	}
	d2, err := img2.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if d1 != d2 {
		t.Errorf("non-reproducible: digest1=%s, digest2=%s", d1, d2)
	}
}

func TestVerifyKVMHarnessDetectsBinaryMismatch(t *testing.T) {
	runtime := fakeRuntimeImage(t)
	created := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	img, err := BuildHarnessImage(runtime, []byte("real"), created, "v1.0.0", "abc")
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyKVMHarness(img, []byte("different"), created, "v1.0.0", "abc"); err == nil {
		t.Fatal("verification passed with wrong binary")
	}
}

func TestPublishKVMRejectsEmptyConfig(t *testing.T) {
	ctx := context.Background()
	config := PublishConfig{Tag: "v1.0.0", SHA: "abc", Dist: t.TempDir()}
	if _, err := PublishKVM(ctx, config, "nonexistent.tar"); err == nil {
		t.Fatal("accepted publish with empty access token")
	}
	config.AccessToken = []byte("token")
	config.Dist = ""
	if _, err := PublishKVM(ctx, config, "nonexistent.tar"); err == nil {
		t.Fatal("accepted publish with empty dist")
	}
}

func TestVerifyKVMRejectsEmptyConfig(t *testing.T) {
	ctx := context.Background()
	config := PublishConfig{Tag: "v1.0.0", SHA: "abc", Dist: t.TempDir()}
	published := KVMPublished{Runtime: "r", Harness: "h"}
	if err := VerifyKVM(ctx, config, published); err == nil {
		t.Fatal("accepted verify with empty access token")
	}
	config.AccessToken = []byte("token")
	published.Runtime = ""
	if err := VerifyKVM(ctx, config, published); err == nil {
		t.Fatal("accepted verify with empty runtime reference")
	}
}

func TestWriteKVMReferences(t *testing.T) {
	dir := t.TempDir()
	published := KVMPublished{
		Runtime: "example.com/rt@sha256:aaa",
		Harness: "example.com/h@sha256:bbb",
	}
	if err := WriteKVMReferences(dir, published); err != nil {
		t.Fatal(err)
	}
	runtime, err := os.ReadFile(filepath.Join(dir, "kvm-runtime-image.txt"))
	if err != nil || string(runtime) != published.Runtime+"\n" {
		t.Errorf("runtime ref = %q, %v", runtime, err)
	}
	harness, err := os.ReadFile(filepath.Join(dir, "kvm-harness-image.txt"))
	if err != nil || string(harness) != published.Harness+"\n" {
		t.Errorf("harness ref = %q, %v", harness, err)
	}
	kvmJSON, err := os.ReadFile(filepath.Join(dir, "kvm-images.json"))
	if err != nil {
		t.Fatal(err)
	}
	var decoded KVMPublished
	if err := json.Unmarshal(kvmJSON, &decoded); err != nil || decoded != published {
		t.Errorf("kvm-images.json = %s, %v", kvmJSON, err)
	}
}

// TestApkoLockfilePinned validates that every package in the lockfile has
// an exact version and a data checksum.
func TestApkoLockfilePinned(t *testing.T) {
	lockPath := filepath.Join("..", "..", "packaging", "kvm-harness", "runtime.apko.lock.json")
	//nolint:gosec // test reads a known repo-relative lockfile.
	body, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("read lockfile: %v", err)
	}
	var lock struct {
		Version  string `json:"version"`
		Contents struct {
			Packages []struct {
				Name    string `json:"name"`
				Version string `json:"version"`
				URL     string `json:"url"`
				Data    struct {
					Checksum string `json:"checksum"`
				} `json:"data"`
			} `json:"packages"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(body, &lock); err != nil {
		t.Fatalf("parse lockfile: %v", err)
	}
	if lock.Version != "v1" {
		t.Errorf("lockfile version = %q, want v1", lock.Version)
	}
	if len(lock.Contents.Packages) == 0 {
		t.Fatal("lockfile has no packages")
	}
	for _, pkg := range lock.Contents.Packages {
		if pkg.Name == "" {
			t.Error("package has empty name")
		}
		if pkg.Version == "" {
			t.Errorf("package %s has no pinned version", pkg.Name)
		}
		if pkg.Data.Checksum == "" {
			t.Errorf("package %s has no data checksum", pkg.Name)
		}
		if pkg.URL == "" {
			t.Errorf("package %s has no URL", pkg.Name)
		}
	}
	// Verify the required packages are present.
	required := map[string]bool{"qemu-system-x86_64": false, "qemu": false, "ca-certificates-bundle": false}
	for _, pkg := range lock.Contents.Packages {
		if _, ok := required[pkg.Name]; ok {
			required[pkg.Name] = true
		}
	}
	for name, found := range required {
		if !found {
			t.Errorf("required package %s missing from lockfile", name)
		}
	}
}
