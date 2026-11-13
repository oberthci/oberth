package releaseimage

import (
	"bytes"
	"debug/elf"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"

	"github.com/oberthci/oberth/internal/staticgit"
)

func mustImageConfig(t *testing.T, image v1.Image, config *v1.ConfigFile) v1.Image {
	t.Helper()
	result, err := mutate.ConfigFile(image, config)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestHelperImageContainsOnlyReleaseBinaryAndRoots(t *testing.T) {
	created := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	for _, arch := range []string{"amd64", "arm64"} {
		t.Run(arch, func(t *testing.T) {
			base := testBaseImage(t, created, map[string]testEntry{
				"usr/local/bin/oberth":              {body: "old", mode: 0755},
				"usr/bin/git":                       {body: "git", mode: 0755},
				"usr/local/bin/oberth-git-ssh":      {link: "oberth", mode: 0777},
				"etc/ssl/certs/ca-certificates.crt": {body: "roots", mode: 0644},
			})
			config, err := base.ConfigFile()
			if err != nil {
				t.Fatal(err)
			}
			config.Architecture = arch
			base = mustImageConfig(t, base, config)
			binary := []byte("exact release binary")
			image, err := Repack(base, Helper, arch, binary, created, "v1.2.3", "revision")
			if err != nil {
				t.Fatal(err)
			}
			files := extractedFiles(t, image)
			if len(files) != 2 || !bytes.Equal(files["usr/local/bin/oberth"], binary) || string(files["etc/ssl/certs/ca-certificates.crt"]) != "roots" {
				t.Fatal("helper retained substrate tools or lost its payload")
			}
			definition, _ := contract(Helper)
			if err := verifyImage(image, definition, arch, binary, created, "v1.2.3", "revision"); err != nil {
				t.Fatal(err)
			}
			if err := verifyImage(image, definition, arch, []byte("wrong"), created, "v1.2.3", "revision"); err == nil {
				t.Fatal("helper accepted different release bytes")
			}
			built, err := image.ConfigFile()
			if err != nil {
				t.Fatal(err)
			}
			for key := range staticgit.Labels() {
				if _, exists := built.Config.Labels[key]; exists {
					t.Fatal("helper advertised an absent static component", key)
				}
			}
		})
	}
}

// Build an OCI image locally from an actual static release executable, then
// reload its archive and execute that exact native payload with its entrypoint.
// This needs neither registry credentials nor a container daemon.
func TestHelperImageRunsNativeReleaseVersion(t *testing.T) {
	if runtime.GOOS != "linux" || (runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64") {
		t.Skip("the helper publishes linux/amd64 and linux/arm64")
	}
	directory := t.TempDir()
	binaryPath := filepath.Join(directory, "oberth")
	build := exec.CommandContext(t.Context(), "go", "build", "-p=2", "-trimpath", "-buildvcs=false", "-ldflags=-s -w -X main.version=v1.2.3", "-o", binaryPath, "../../cmd/oberth")
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build release binary: %v\n%s", err, out)
	}
	binary, err := os.ReadFile(binaryPath)
	if err != nil {
		t.Fatal(err)
	}
	executable, err := elf.NewFile(bytes.NewReader(binary))
	if err != nil {
		t.Fatal(err)
	}
	defer executable.Close()
	for _, program := range executable.Progs {
		if program.Type == elf.PT_INTERP {
			t.Fatal("helper executable requires a dynamic loader")
		}
	}
	roots, err := os.ReadFile("/etc/ssl/certs/ca-certificates.crt")
	if err != nil {
		t.Fatal(err)
	}
	created := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	base := testBaseImage(t, created, map[string]testEntry{
		"usr/local/bin/oberth":              {body: "old", mode: 0755},
		"etc/ssl/certs/ca-certificates.crt": {body: string(roots), mode: 0644},
	})
	config, err := base.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	config.Architecture = runtime.GOARCH
	image, err := Repack(mustImageConfig(t, base, config), Helper, runtime.GOARCH, binary, created, "v1.2.3", "revision")
	if err != nil {
		t.Fatal(err)
	}
	reference, err := name.NewTag("oberth-helper:test")
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(directory, "helper.tar")
	if err := tarball.WriteToFile(archive, reference, image); err != nil {
		t.Fatal(err)
	}
	loaded, err := tarball.ImageFromPath(archive, &reference)
	if err != nil {
		t.Fatal(err)
	}
	loadedConfig, err := loaded.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	if len(loadedConfig.Config.Entrypoint) != 1 || loadedConfig.Config.Entrypoint[0] != "/usr/local/bin/oberth" || loadedConfig.Config.WorkingDir != "/" {
		t.Fatal("helper image cannot invoke its executable", loadedConfig.Config)
	}
	files := extractedFiles(t, loaded)
	if len(files) != 2 || !bytes.Equal(files["usr/local/bin/oberth"], binary) || !bytes.Equal(files["etc/ssl/certs/ca-certificates.crt"], roots) {
		t.Fatal("image archive changed the release payload")
	}
	entry := strings.TrimPrefix(loadedConfig.Config.Entrypoint[0], "/")
	payload := filepath.Join(directory, "rootfs", entry)
	if err := os.MkdirAll(filepath.Dir(payload), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(payload, files[entry], 0755); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), payload, "version")
	command.Env = []string{"PATH=/usr/local/bin", "HOME=" + directory}
	out, err := command.CombinedOutput()
	if err != nil || !strings.HasPrefix(string(out), "oberth v1.2.3") {
		t.Fatalf("image payload version: %v %s", err, out)
	}
}
