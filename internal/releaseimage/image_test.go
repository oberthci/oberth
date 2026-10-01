package releaseimage

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

func TestRepackRemovesPriorOberthAndAddsOnlyExactNewExecutable(t *testing.T) {
	created := time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC)
	base := testBaseImage(t, created, map[string]testEntry{
		"usr/local/bin/oberth":              {body: "old executable", mode: 0o755},
		"usr/bin/git":                       {body: "git", mode: 0o755},
		"usr/bin/ssh":                       {body: "ssh", mode: 0o755},
		"etc/ssl/certs/ca-certificates.crt": {body: "roots", mode: 0o644},
	})
	newBinary := []byte("new exact executable")
	image, err := Repack(base, Server, "amd64", newBinary, created, "v1.2.3", "0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	files := extractedFiles(t, image)
	if got := files["usr/local/bin/oberth"]; !bytes.Equal(got, newBinary) {
		t.Fatalf("released executable = %q", got)
	}
	if bytes.Contains(bytes.Join(mapValues(files), nil), []byte("old executable")) {
		t.Fatal("prior Oberth executable leaked into the clean release image")
	}
	config, err := image.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	if config.Config.User != "65534:65534" || len(config.Config.Entrypoint) != 1 || config.Config.Entrypoint[0] != "/usr/local/bin/oberth" {
		t.Fatalf("release image config = %#v", config.Config)
	}
}

func TestRepackRejectsSubstrateWithoutExpectedExecutable(t *testing.T) {
	created := time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC)
	base := testBaseImage(t, created, map[string]testEntry{
		"usr/bin/git":                       {body: "git", mode: 0o755},
		"usr/bin/ssh":                       {body: "ssh", mode: 0o755},
		"etc/ssl/certs/ca-certificates.crt": {body: "roots", mode: 0o644},
	})
	if _, err := Repack(base, Server, "amd64", []byte("new"), created, "v1.2.3", "abc"); err == nil {
		t.Fatal("substrate without prior executable was accepted")
	}
}

func TestVerifyImageBindsLayerConfigAndExactExecutable(t *testing.T) {
	created := time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC)
	base := testBaseImage(t, created, map[string]testEntry{
		"usr/local/bin/oberth":              {body: "prior server", mode: 0o755},
		"usr/bin/git":                       {body: "git", mode: 0o755},
		"usr/bin/ssh":                       {body: "ssh", mode: 0o755},
		"etc/ssl/certs/ca-certificates.crt": {body: "roots", mode: 0o644},
	})
	binary := []byte("new server")
	image, err := Repack(base, Server, "amd64", binary, created, "v1.2.3", "0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	definition, _ := contract(Server)
	if err := verifyImage(image, definition, "amd64", binary, created, "v1.2.3", "0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	if err := verifyImage(image, definition, "amd64", []byte("different"), created, "v1.2.3", "0123456789abcdef"); err == nil {
		t.Fatal("different local executable was accepted")
	}
	config, err := image.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	config.Config.User = "0:0"
	tampered, err := mutate.ConfigFile(image, config)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyImage(tampered, definition, "amd64", binary, created, "v1.2.3", "0123456789abcdef"); err == nil {
		t.Fatal("tampered runtime configuration was accepted")
	}
}

func TestRepackAcceptsArm64SubstrateForArm64Release(t *testing.T) {
	created := time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC)
	base := testBaseImage(t, created, map[string]testEntry{
		"usr/local/bin/oberth":              {body: "prior server", mode: 0o755},
		"usr/bin/git":                       {body: "git", mode: 0o755},
		"usr/bin/ssh":                       {body: "ssh", mode: 0o755},
		"etc/ssl/certs/ca-certificates.crt": {body: "roots", mode: 0o644},
	})
	config, err := base.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	config.Architecture = "arm64"
	base, err = mutate.ConfigFile(base, config)
	if err != nil {
		t.Fatal(err)
	}
	binary := []byte("arm64 server")
	image, err := Repack(base, Server, "arm64", binary, created, "v1.2.3", "0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	definition, _ := contract(Server)
	if err := verifyImage(image, definition, "arm64", binary, created, "v1.2.3", "0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	releaseConfig, err := image.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	if releaseConfig.Architecture != "arm64" || releaseConfig.OS != "linux" {
		t.Fatalf("release image platform = %s/%s, want linux/arm64", releaseConfig.OS, releaseConfig.Architecture)
	}
}

func TestContractsRequireBothReleasePlatforms(t *testing.T) {
	definition, err := contract(Server)
	if err != nil {
		t.Fatal(err)
	}
	if len(definition.platforms) != 2 || definition.platforms[0] != "amd64" || definition.platforms[1] != "arm64" {
		t.Fatalf("server release platforms = %v, want [amd64 arm64]", definition.platforms)
	}
	// The runner image is retired: only the server contract exists, and any
	// other kind fails closed.
	if _, err := contract(Kind("runner")); err == nil {
		t.Fatal("retired runner image contract still exists")
	}
}

func TestRepackRejectsArchitectureWithoutMatchingSubstrate(t *testing.T) {
	created := time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC)
	base := testBaseImage(t, created, map[string]testEntry{
		"usr/local/bin/oberth":              {body: "prior server", mode: 0o755},
		"usr/bin/git":                       {body: "git", mode: 0o755},
		"usr/bin/ssh":                       {body: "ssh", mode: 0o755},
		"etc/ssl/certs/ca-certificates.crt": {body: "roots", mode: 0o644},
	})
	if _, err := Repack(base, Server, "arm64", []byte("arm64 server"), created, "v1.2.3", "0123456789abcdef"); err == nil {
		t.Fatal("amd64-only server substrate was relabeled as arm64")
	}
}

func TestRepackRejectsSubstrateWhoseConfigDoesNotMatchRequestedPlatform(t *testing.T) {
	created := time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC)
	base := testBaseImage(t, created, map[string]testEntry{
		"usr/local/bin/oberth":              {body: "prior server", mode: 0o755},
		"usr/bin/git":                       {body: "git", mode: 0o755},
		"usr/bin/ssh":                       {body: "ssh", mode: 0o755},
		"etc/ssl/certs/ca-certificates.crt": {body: "roots", mode: 0o644},
	})
	config, err := base.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	config.Architecture = "arm64"
	base, err = mutate.ConfigFile(base, config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Repack(base, Server, "amd64", []byte("amd64 server"), created, "v1.2.3", "0123456789abcdef"); err == nil {
		t.Fatal("arm64 substrate config was relabeled as amd64")
	}
}

func TestForbiddenPathMatchesPrefixesAndWildcards(t *testing.T) {
	prefixes := []string{"seed", "usr/local/go", "usr/bin/python*"}
	for path, want := range map[string]bool{
		"seed":                 true,
		"seed/tools/apiserver": true,
		"seeded/file":          false,
		"usr/local/go/bin/go":  true,
		"usr/bin/python3":      true,
		"usr/bin/py":           false,
		"usr/local/bin/oberth": false,
	} {
		if got := forbiddenPath(path, prefixes); got != want {
			t.Fatalf("forbiddenPath(%q) = %v, want %v", path, got, want)
		}
	}
}

type testEntry struct {
	body string
	mode int64
}

func testBaseImage(t *testing.T, created time.Time, files map[string]testEntry) v1.Image {
	t.Helper()
	var body bytes.Buffer
	writer := tar.NewWriter(&body)
	for name, entry := range files {
		if err := writer.WriteHeader(&tar.Header{Name: name, Mode: entry.mode, Size: int64(len(entry.body)), ModTime: created}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte(entry.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	layerBytes := bytes.Clone(body.Bytes())
	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(layerBytes)), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	image, err := mutate.AppendLayers(empty.Image, layer)
	if err != nil {
		t.Fatal(err)
	}
	config, err := image.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	config.OS = "linux"
	config.Architecture = "amd64"
	image, err = mutate.ConfigFile(image, config)
	if err != nil {
		t.Fatal(err)
	}
	return image
}

func extractedFiles(t *testing.T, image v1.Image) map[string][]byte {
	t.Helper()
	reader := mutate.Extract(image)
	defer func() { _ = reader.Close() }()
	archive := tar.NewReader(reader)
	files := make(map[string][]byte)
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			return files
		}
		if err != nil {
			t.Fatal(err)
		}
		if !regularTarType(header.Typeflag) {
			continue
		}
		body, err := io.ReadAll(archive)
		if err != nil {
			t.Fatal(err)
		}
		files[header.Name] = body
	}
}

func mapValues(values map[string][]byte) [][]byte {
	result := make([][]byte, 0, len(values))
	for _, value := range values {
		result = append(result, value)
	}
	return result
}

func TestIsNotFound(t *testing.T) {
	t.Run("transport 404", func(t *testing.T) {
		err := &transport.Error{StatusCode: http.StatusNotFound}
		if !isNotFound(err) {
			t.Fatal("404 transport.Error not recognized as not-found")
		}
	})
	t.Run("transport 403", func(t *testing.T) {
		err := &transport.Error{StatusCode: http.StatusForbidden}
		if isNotFound(err) {
			t.Fatal("403 transport.Error falsely recognized as not-found")
		}
	})
	t.Run("transport 500", func(t *testing.T) {
		err := &transport.Error{StatusCode: http.StatusInternalServerError}
		if isNotFound(err) {
			t.Fatal("500 transport.Error falsely recognized as not-found")
		}
	})
	t.Run("wrapped transport 404", func(t *testing.T) {
		inner := &transport.Error{StatusCode: http.StatusNotFound}
		wrapped := fmt.Errorf("outer: %w", inner)
		if !isNotFound(wrapped) {
			t.Fatal("wrapped 404 transport.Error not recognized as not-found")
		}
	})
	t.Run("generic error", func(t *testing.T) {
		if isNotFound(errors.New("not found")) {
			t.Fatal("plain error falsely recognized as not-found")
		}
	})
	t.Run("nil error", func(t *testing.T) {
		if isNotFound(nil) {
			t.Fatal("nil error falsely recognized as not-found")
		}
	})
}

func TestPublishRejectsEmptyConfig(t *testing.T) {
	ctx := context.Background()
	t.Run("empty key", func(t *testing.T) {
		config := PublishConfig{Tag: "v1.0.0", SHA: "abc", Dist: "/tmp/dist"}
		if _, err := Publish(ctx, config); err == nil {
			t.Fatal("accepted Publish with empty key")
		}
	})
	t.Run("empty dist", func(t *testing.T) {
		config := PublishConfig{Tag: "v1.0.0", SHA: "abc", AccessToken: []byte(`{"key":"value"}`)}
		if _, err := Publish(ctx, config); err == nil {
			t.Fatal("accepted Publish with empty dist")
		}
	})
	t.Run("both empty", func(t *testing.T) {
		config := PublishConfig{Tag: "v1.0.0", SHA: "abc"}
		if _, err := Publish(ctx, config); err == nil {
			t.Fatal("accepted Publish with empty key and dist")
		}
	})
}

func TestVerifyRejectsEmptyConfig(t *testing.T) {
	ctx := context.Background()
	t.Run("empty key", func(t *testing.T) {
		config := PublishConfig{Tag: "v1.0.0", SHA: "abc", Dist: "/tmp/dist"}
		published := Published{Server: "example.com/img@sha256:abcd"}
		if err := Verify(ctx, config, published); err == nil {
			t.Fatal("accepted Verify with empty key")
		}
	})
	t.Run("empty dist", func(t *testing.T) {
		config := PublishConfig{Tag: "v1.0.0", SHA: "abc", AccessToken: []byte(`{"key":"value"}`)}
		published := Published{Server: "example.com/img@sha256:abcd"}
		if err := Verify(ctx, config, published); err == nil {
			t.Fatal("accepted Verify with empty dist")
		}
	})
	t.Run("empty server reference", func(t *testing.T) {
		config := PublishConfig{Tag: "v1.0.0", SHA: "abc", AccessToken: []byte(`{"key":"value"}`), Dist: "/tmp/dist"}
		published := Published{Server: ""}
		if err := Verify(ctx, config, published); err == nil {
			t.Fatal("accepted Verify with empty server reference")
		}
	})
}

func TestCleanTarNameEdgeCases(t *testing.T) {
	for _, tc := range []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "simple path", input: "usr/bin/git", want: "usr/bin/git"},
		{name: "dotslash prefix", input: "./usr/bin/git", want: "usr/bin/git"},
		{name: "absolute path", input: "/usr/bin/git", wantErr: true},
		{name: "NUL byte", input: "usr/\x00bin", wantErr: true},
		{name: "parent traversal", input: "../etc/passwd", wantErr: true},
		{name: "dot-only", input: ".", wantErr: true},
		{name: "dotslash-only", input: "./", wantErr: true},
		{name: "double dot", input: "..", wantErr: true},
		{name: "embedded parent", input: "usr/../etc/passwd", want: "etc/passwd"},
		{name: "redundant slashes", input: "usr//bin//git", want: "usr/bin/git"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := cleanTarName(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("cleanTarName(%q) = %q, want error", tc.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("cleanTarName(%q) error: %v", tc.input, err)
			}
			if got != tc.want {
				t.Fatalf("cleanTarName(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestRepackRejectsEmptyBinary(t *testing.T) {
	created := time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC)
	base := testBaseImage(t, created, map[string]testEntry{
		"usr/local/bin/oberth":              {body: "old executable", mode: 0o755},
		"usr/bin/git":                       {body: "git", mode: 0o755},
		"usr/bin/ssh":                       {body: "ssh", mode: 0o755},
		"etc/ssl/certs/ca-certificates.crt": {body: "roots", mode: 0o644},
	})
	if _, err := Repack(base, Server, "amd64", nil, created, "v1.2.3", "abc"); err == nil {
		t.Fatal("accepted nil binary")
	}
	if _, err := Repack(base, Server, "amd64", []byte{}, created, "v1.2.3", "abc"); err == nil {
		t.Fatal("accepted empty binary")
	}
}

func TestRepackRejectsZeroCreatedTime(t *testing.T) {
	base := testBaseImage(t, time.Now(), map[string]testEntry{
		"usr/local/bin/oberth":              {body: "old executable", mode: 0o755},
		"usr/bin/git":                       {body: "git", mode: 0o755},
		"usr/bin/ssh":                       {body: "ssh", mode: 0o755},
		"etc/ssl/certs/ca-certificates.crt": {body: "roots", mode: 0o644},
	})
	if _, err := Repack(base, Server, "amd64", []byte("binary"), time.Time{}, "v1.2.3", "abc"); err == nil {
		t.Fatal("accepted zero creation time")
	}
}

func TestRepackRejectsUnknownKind(t *testing.T) {
	created := time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC)
	base := testBaseImage(t, created, map[string]testEntry{
		"usr/local/bin/oberth": {body: "old executable", mode: 0o755},
	})
	if _, err := Repack(base, Kind("alien"), "amd64", []byte("binary"), created, "v1.2.3", "abc"); err == nil {
		t.Fatal("accepted unknown image kind")
	}
}

func TestRepackRejectsSubstrateMissingRequiredPath(t *testing.T) {
	created := time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC)
	base := testBaseImage(t, created, map[string]testEntry{
		"usr/local/bin/oberth": {body: "old executable", mode: 0o755},
		"usr/bin/git":          {body: "git", mode: 0o755},
		// Missing: usr/bin/ssh and etc/ssl/certs/ca-certificates.crt
	})
	if _, err := Repack(base, Server, "amd64", []byte("binary"), created, "v1.2.3", "abc"); err == nil {
		t.Fatal("accepted substrate missing required paths")
	}
}

func TestRegularTarType(t *testing.T) {
	if !regularTarType(tar.TypeReg) {
		t.Fatal("TypeReg not recognized as regular")
	}
	if !regularTarType(0) {
		t.Fatal("zero typeflag not recognized as regular")
	}
	if regularTarType(tar.TypeDir) {
		t.Fatal("TypeDir falsely recognized as regular")
	}
	if regularTarType(tar.TypeSymlink) {
		t.Fatal("TypeSymlink falsely recognized as regular")
	}
}

func TestRepackRejectsUnsupportedArchitecture(t *testing.T) {
	created := time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC)
	base := testBaseImage(t, created, map[string]testEntry{
		"usr/local/bin/oberth":              {body: "old executable", mode: 0o755},
		"usr/bin/git":                       {body: "git", mode: 0o755},
		"usr/bin/ssh":                       {body: "ssh", mode: 0o755},
		"etc/ssl/certs/ca-certificates.crt": {body: "roots", mode: 0o644},
	})
	// riscv64 is not in the server platform list
	if _, err := Repack(base, Server, "riscv64", []byte("binary"), created, "v1.2.3", "abc"); err == nil {
		t.Fatal("accepted unsupported architecture riscv64")
	}
}

func TestVerifyImageRejectsMultiLayerImage(t *testing.T) {
	created := time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC)
	// Build a two-layer image manually
	layer1 := testTarLayer(t, map[string]testEntry{
		"usr/bin/git": {body: "git", mode: 0o755},
	})
	layer2 := testTarLayer(t, map[string]testEntry{
		"usr/local/bin/oberth": {body: "server", mode: 0o755},
	})
	image, err := mutate.AppendLayers(empty.Image, layer1, layer2)
	if err != nil {
		t.Fatal(err)
	}
	config, err := image.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	config.OS = "linux"
	config.Architecture = "amd64"
	image, err = mutate.ConfigFile(image, config)
	if err != nil {
		t.Fatal(err)
	}
	definition, _ := contract(Server)
	if err := verifyImage(image, definition, "amd64", []byte("server"), created, "v1.0.0", "abc"); err == nil {
		t.Fatal("accepted multi-layer release image")
	}
}

func TestVerifyImageRejectsMissingExecutable(t *testing.T) {
	created := time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC)
	// Build a single-layer image without the expected executable
	layer := testTarLayer(t, map[string]testEntry{
		"usr/bin/git":                       {body: "git", mode: 0o755},
		"usr/bin/ssh":                       {body: "ssh", mode: 0o755},
		"etc/ssl/certs/ca-certificates.crt": {body: "roots", mode: 0o644},
	})
	image, err := mutate.AppendLayers(empty.Image, layer)
	if err != nil {
		t.Fatal(err)
	}
	config, err := image.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	config.OS = "linux"
	config.Architecture = "amd64"
	config.Created = v1.Time{Time: created}
	config.Author = "Oberth"
	definition, _ := contract(Server)
	config.Config = definition.config
	config.Config.Labels = map[string]string{
		"org.opencontainers.image.created":  created.Format("2006-01-02T15:04:05Z07:00"),
		"org.opencontainers.image.revision": "abc",
		"org.opencontainers.image.source":   "https://github.com/oberthci/oberth",
		"org.opencontainers.image.version":  "v1.0.0",
	}
	config.History = []v1.History{{
		Created: v1.Time{Time: created}, CreatedBy: "oberth repository release", Comment: "clean flattened substrate plus exact source-built executable",
	}}
	image, err = mutate.ConfigFile(image, config)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyImage(image, definition, "amd64", []byte("server"), created, "v1.0.0", "abc"); err == nil {
		t.Fatal("accepted image missing release executable")
	}
}

func TestVerifyImageRejectsNonExecutablePermissions(t *testing.T) {
	created := time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC)
	// Build image with the executable at mode 0644 (not executable)
	layer := testTarLayer(t, map[string]testEntry{
		"usr/local/bin/oberth":              {body: "server", mode: 0o644}, // Not executable
		"usr/bin/git":                       {body: "git", mode: 0o755},
		"usr/bin/ssh":                       {body: "ssh", mode: 0o755},
		"etc/ssl/certs/ca-certificates.crt": {body: "roots", mode: 0o644},
	})
	image, err := mutate.AppendLayers(empty.Image, layer)
	if err != nil {
		t.Fatal(err)
	}
	config, err := image.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	config.OS = "linux"
	config.Architecture = "amd64"
	config.Created = v1.Time{Time: created}
	config.Author = "Oberth"
	definition, _ := contract(Server)
	config.Config = definition.config
	config.Config.Labels = map[string]string{
		"org.opencontainers.image.created":  created.Format("2006-01-02T15:04:05Z07:00"),
		"org.opencontainers.image.revision": "abc",
		"org.opencontainers.image.source":   "https://github.com/oberthci/oberth",
		"org.opencontainers.image.version":  "v1.0.0",
	}
	config.History = []v1.History{{
		Created: v1.Time{Time: created}, CreatedBy: "oberth repository release", Comment: "clean flattened substrate plus exact source-built executable",
	}}
	image, err = mutate.ConfigFile(image, config)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyImage(image, definition, "amd64", []byte("server"), created, "v1.0.0", "abc"); err == nil {
		t.Fatal("accepted non-executable release binary")
	}
}

func TestVerifyImageRejectsMissingRequiredBootstrapPath(t *testing.T) {
	created := time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC)
	// Build image with executable but missing required bootstrap paths
	layer := testTarLayer(t, map[string]testEntry{
		"usr/local/bin/oberth": {body: "server", mode: 0o755},
		// Missing: usr/bin/git, usr/bin/ssh, etc/ssl/certs/ca-certificates.crt
	})
	image, err := mutate.AppendLayers(empty.Image, layer)
	if err != nil {
		t.Fatal(err)
	}
	config, err := image.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	config.OS = "linux"
	config.Architecture = "amd64"
	config.Created = v1.Time{Time: created}
	config.Author = "Oberth"
	definition, _ := contract(Server)
	config.Config = definition.config
	config.Config.Labels = map[string]string{
		"org.opencontainers.image.created":  created.Format("2006-01-02T15:04:05Z07:00"),
		"org.opencontainers.image.revision": "abc",
		"org.opencontainers.image.source":   "https://github.com/oberthci/oberth",
		"org.opencontainers.image.version":  "v1.0.0",
	}
	config.History = []v1.History{{
		Created: v1.Time{Time: created}, CreatedBy: "oberth repository release", Comment: "clean flattened substrate plus exact source-built executable",
	}}
	image, err = mutate.ConfigFile(image, config)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyImage(image, definition, "amd64", []byte("server"), created, "v1.0.0", "abc"); err == nil {
		t.Fatal("accepted image missing required bootstrap paths")
	}
}

func testTarLayer(t *testing.T, files map[string]testEntry) v1.Layer {
	t.Helper()
	var body bytes.Buffer
	writer := tar.NewWriter(&body)
	for name, entry := range files {
		if err := writer.WriteHeader(&tar.Header{
			Name: name, Mode: entry.mode, Size: int64(len(entry.body)),
			Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte(entry.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	layerBytes := bytes.Clone(body.Bytes())
	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(layerBytes)), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return layer
}
