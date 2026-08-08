package releaseimage

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
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
		"usr/local/bin/oberth-runner":       {body: "prior runner", mode: 0o755},
		"bin/sh":                            {body: "shell", mode: 0o755},
		"etc/ssl/certs/ca-certificates.crt": {body: "roots", mode: 0o644},
		"usr/bin/curl":                      {body: "curl", mode: 0o755},
		"usr/bin/git":                       {body: "git", mode: 0o755},
		"cache/gobuild":                     {mode: 0o755},
		"cache/gomod":                       {mode: 0o755},
		"secrets":                           {mode: 0o755},
		"tmp/oberth-tools/bin":              {mode: 0o755},
		"work":                              {mode: 0o755},
	})
	binary := []byte("new runner")
	image, err := Repack(base, Runner, "amd64", binary, created, "v1.2.3", "0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	definition, _ := contract(Runner)
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

func TestRepackRejectsArchitectureWithoutMatchingSubstrate(t *testing.T) {
	created := time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC)
	base := testBaseImage(t, created, map[string]testEntry{
		"usr/local/bin/oberth-runner":       {body: "prior runner", mode: 0o755},
		"bin/sh":                            {body: "shell", mode: 0o755},
		"etc/ssl/certs/ca-certificates.crt": {body: "roots", mode: 0o644},
		"usr/bin/curl":                      {body: "curl", mode: 0o755},
		"usr/bin/git":                       {body: "git", mode: 0o755},
		"cache/gobuild":                     {mode: 0o755},
		"cache/gomod":                       {mode: 0o755},
		"secrets":                           {mode: 0o755},
		"tmp/oberth-tools/bin":              {mode: 0o755},
		"work":                              {mode: 0o755},
	})
	if _, err := Repack(base, Runner, "arm64", []byte("arm64 runner"), created, "v1.2.3", "0123456789abcdef"); err == nil {
		t.Fatal("amd64-only runner substrate was relabeled as arm64")
	}
}

func TestRepackRejectsSubstrateWhoseConfigDoesNotMatchRequestedPlatform(t *testing.T) {
	created := time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC)
	base := testBaseImage(t, created, map[string]testEntry{
		"usr/local/bin/oberth-runner":       {body: "prior runner", mode: 0o755},
		"bin/sh":                            {body: "shell", mode: 0o755},
		"etc/ssl/certs/ca-certificates.crt": {body: "roots", mode: 0o644},
		"usr/bin/curl":                      {body: "curl", mode: 0o755},
		"usr/bin/git":                       {body: "git", mode: 0o755},
		"cache/gobuild":                     {mode: 0o755},
		"cache/gomod":                       {mode: 0o755},
		"secrets":                           {mode: 0o755},
		"tmp/oberth-tools/bin":              {mode: 0o755},
		"work":                              {mode: 0o755},
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
	if _, err := Repack(base, Runner, "amd64", []byte("amd64 runner"), created, "v1.2.3", "0123456789abcdef"); err == nil {
		t.Fatal("arm64 substrate config was relabeled as amd64")
	}
}

func TestRepackRejectsRunnerSubstrateWithRepositoryBuildDependencies(t *testing.T) {
	created := time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC)
	for _, forbidden := range []string{
		"seed/tools/kube-apiserver",
		"usr/local/go/bin/go",
		"usr/local/bin/golangci-lint",
		"usr/local/bin/helm",
		"usr/local/bin/trivy",
		"usr/bin/xz",
		"usr/bin/python3",
		"usr/lib/python3.13/os.py",
		"usr/local/bin/python3.13",
		"usr/local/lib/libpython3.13.so.1.0",
		"var/log/apk.log",
	} {
		t.Run(forbidden, func(t *testing.T) {
			base := testBaseImage(t, created, map[string]testEntry{
				"usr/local/bin/oberth-runner":       {body: "prior runner", mode: 0o755},
				"bin/sh":                            {body: "shell", mode: 0o755},
				"etc/ssl/certs/ca-certificates.crt": {body: "roots", mode: 0o644},
				"usr/bin/curl":                      {body: "curl", mode: 0o755},
				"usr/bin/git":                       {body: "git", mode: 0o755},
				"cache/gobuild":                     {mode: 0o755},
				"cache/gomod":                       {mode: 0o755},
				"secrets":                           {mode: 0o755},
				"tmp/oberth-tools/bin":              {mode: 0o755},
				"work":                              {mode: 0o755},
				forbidden:                           {body: "baked dependency", mode: 0o755},
			})
			if _, err := Repack(base, Runner, "amd64", []byte("new runner"), created, "v1.2.3", "0123456789abcdef"); err == nil || !strings.Contains(err.Error(), forbidden) {
				t.Fatalf("runner substrate containing %q error = %v", forbidden, err)
			}
		})
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
