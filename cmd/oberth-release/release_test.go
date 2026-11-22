package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/google/go-containerregistry/pkg/v1/validate"
	"github.com/oberthci/oberth/internal/staticgit"
)

func setup(t *testing.T) options {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"binaries", "chart", "dist"} {
		if err := os.Mkdir(filepath.Join(dir, name), 0755); err != nil {
			t.Fatal(err)
		}
	}
	source := filepath.Join(dir, "fixture.go")
	if err := os.WriteFile(source, []byte("package main\nfunc main() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"darwin-amd64", "darwin-arm64", "linux-amd64", "linux-arm64"} {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		cmd := exec.CommandContext(ctx, "go", "build", "-p=1", "-buildvcs=false", "-trimpath", "-ldflags=-buildid=", "-o", filepath.Join(dir, "binaries", "oberth-"+target), source)
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+strings.Split(target, "-")[0], "GOARCH="+strings.Split(target, "-")[1])
		output, err := cmd.CombinedOutput()
		cancel()
		if err != nil {
			t.Fatalf("build %s: %v: %s", target, err, output)
		}
	}
	for _, arch := range []string{"amd64", "arm64"} {
		copyFile(t, filepath.Join(dir, "binaries", "oberth-linux-"+arch), filepath.Join(dir, "binaries", "oberth-source-plugin-linux-"+arch))
	}
	o := options{dir: filepath.Join(dir, "dist"), base: filepath.Join(dir, "base"), pin: filepath.Join(dir, "base.digest"), chart: filepath.Join(dir, "chart"), version: "v1.2.3", commit: strings.Repeat("a", 40), git: fixturePin}
	for name, body := range map[string]string{"Chart.yaml": "apiVersion: v2\nname: oberth\nversion: 0.0.0\nappVersion: unset\n", "values.yaml": "imageDigest: unset\nsourcePluginImageDigest: unset\n"} {
		if err := os.WriteFile(filepath.Join(o.chart, name), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	setupBase(t, o)
	return o
}
func fixtureGit(arch string) []byte {
	body := make([]byte, 64)
	copy(body, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	binary.LittleEndian.PutUint16(body[16:], 2)
	machine := uint16(62)
	if arch == "arm64" {
		machine = 183
	}
	binary.LittleEndian.PutUint16(body[18:], machine)
	binary.LittleEndian.PutUint32(body[20:], 1)
	binary.LittleEndian.PutUint16(body[52:], 64)
	return body
}
func fixturePin(arch staticgit.Architecture) (staticgit.Record, error) {
	pin, err := staticgit.Qualified(arch)
	pin.GitSHA256 = strings.TrimPrefix(digest(fixtureGit(string(arch))), "sha256:")
	return pin, err
}
func copyFile(t *testing.T, from, to string) {
	t.Helper()
	body, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, body, 0755); err != nil {
		t.Fatal(err)
	}
}
func packageFixture(t *testing.T, o options) []byte {
	t.Helper()
	for _, name := range binaryNames() {
		copyFile(t, filepath.Join(filepath.Dir(o.dir), "binaries", name), filepath.Join(o.dir, name))
	}
	if err := packageRelease(t.Context(), o); err != nil {
		t.Fatal(err)
	}
	sums, err := os.ReadFile(filepath.Join(o.dir, "SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}
	return sums
}

// Covers: REQ-C13-003
func TestReproduciblePackagedBytesContract(t *testing.T) {
	first := setup(t)
	a := packageFixture(t, first)
	second := setup(t)
	b := packageFixture(t, second)
	if !bytes.Equal(a, b) {
		t.Fatalf("two independent fixture builds differ:\n%s\n%s", a, b)
	}
	body, err := os.ReadFile(filepath.Join(first.dir, "oberth.oci", "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	var index v1.IndexManifest
	if err := json.Unmarshal(body, &index); err != nil {
		t.Fatal(err)
	}
	if len(index.Manifests) != 1 || index.Manifests[0].Annotations["org.opencontainers.image.ref.name"] != first.version || index.Manifests[0].MediaType != types.OCIImageIndex {
		t.Fatal("missing target")
	}
	// Layout contents are loaded through the same standard image validator below.
	assertImageInventory(t, first, "oberth")
	if err := os.WriteFile(first.pin, []byte("sha256:"+strings.Repeat("0", 64)), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := checkedBase(first); err == nil {
		t.Fatal("changed base pin accepted")
	}
}

func TestImageRefusesUnpinnedGit(t *testing.T) {
	o := setup(t)
	index, err := checkedBase(o)
	if err != nil {
		t.Fatal(err)
	}
	image, err := baseImage(index, "amd64")
	if err != nil {
		t.Fatal(err)
	}
	pin, err := staticgit.Qualified(staticgit.AMD64)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := baseFiles(image, pin); err == nil {
		t.Fatal("unqualified git accepted")
	}
	pin, err = fixturePin(staticgit.AMD64)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := baseFiles(image, pin); err != nil {
		t.Fatal(err)
	}
	// The command root always supplies the reviewed pins.
	if err := run(context.Background(), []string{"base", "--base", o.base, "--pin", o.pin}); err == nil {
		t.Fatal("command accepted fixture git as reviewed")
	}
}

func TestRecordListsExactlyShippedBinaries(t *testing.T) {
	o := setup(t)
	sums := packageFixture(t, o)
	for _, image := range []string{"oberth", "oberth-source-plugin"} {
		assertImageInventory(t, o, image)
	}
	var record struct {
		Components map[string]any `json:"components"`
	}
	body, err := os.ReadFile(filepath.Join(o.dir, "release.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body, &record); err != nil {
		t.Fatal(err)
	}
	if len(record.Components) != 6 || bytes.Contains(sums, []byte(".oci")) || bytes.Contains(body, []byte(`"files"`)) {
		t.Fatal("published inventory is incorrect")
	}
}
func assertImageInventory(t *testing.T, o options, name string) {
	t.Helper()
	err := withImageIndex(o, name, func(index v1.ImageIndex) error {
		manifest, err := index.IndexManifest()
		if err != nil {
			return err
		}
		if manifest.MediaType != types.OCIImageIndex || len(manifest.Manifests) != 2 {
			return fmt.Errorf("invalid image index")
		}
		if err := validate.Index(index); err != nil {
			return err
		}
		for _, arch := range []string{"amd64", "arm64"} {
			image, err := baseImage(index, arch)
			if err != nil {
				t.Fatal(err)
			}
			assertPlatformContents(t, o, image, name+"-linux-"+arch)
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func assertPlatformContents(t *testing.T, o options, image v1.Image, binaryName string) {
	t.Helper()
	name := strings.Split(binaryName, "-linux-")[0]
	if err := ociImage(image); err != nil {
		t.Fatal(err)
	}
	reader := mutate.Extract(image)
	contents, err := readArchive(reader)
	closeErr := reader.Close()
	if err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}
	shipped, err := os.ReadFile(filepath.Join(o.dir, binaryName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(contents["usr/local/bin/"+name], shipped) {
		t.Fatal("image differs from shipped binary")
	}
	for path, body := range contents {
		if _, err := buildinfo.Read(bytes.NewReader(body)); err == nil && path != "usr/local/bin/"+name {
			t.Fatal("extra image binary")
		}
	}
}

func TestCanonicalArchive(t *testing.T) {
	data, err := archive(map[string][]byte{"z": []byte("z"), "a": []byte("a")}, true)
	if err != nil {
		t.Fatal(err)
	}
	reader := tar.NewReader(bytes.NewReader(data))
	for _, name := range []string{"a", "z"} {
		h, err := reader.Next()
		if err != nil {
			t.Fatal(err)
		}
		if h.Name != name || h.Mode != 0555 || h.Uid != 0 || h.Gid != 0 || h.ModTime.Unix() != 0 {
			t.Fatalf("noncanonical: %+v", h)
		}
	}
	if _, err := reader.Next(); err != io.EOF {
		t.Fatal(err)
	}
}

func TestReleaseCommandMessages(t *testing.T) {
	for _, r := range []refusal{imageMissing, imagePrivate, imageRepositories, registryResponse, registryUnavailable, signingKeyMismatch, keyInvalid, keyReplace, keyPinInvalid, keyMarkers, failure, usage, releaseToken, inputs, basePin, baseMismatch, chartStamp, binariesMismatch, latestInvalid, storeFailure, tagMissing, assetUnconfirmed, immutable, notVerified, tokenShared, tokenInside, assetsDiffer, originDiffers} {
		if len(r.Error()) > 80 || len(r.Remedy()) > 120 || r.Remedy() == "" {
			t.Fatal(r)
		}
	}
	for _, args := range [][]string{nil, {"unknown"}, {"verify", "--bad"}, {"verify", "extra"}, {"base", "--base", "missing"}} {
		if run(context.Background(), args) == nil {
			t.Fatal("bad input accepted", args)
		}
	}
}

func TestBaseGraphIsValid(t *testing.T) {
	o := setup(t)
	index, err := checkedBase(o)
	if err != nil {
		t.Fatal(err)
	}
	if err := validate.Index(index); err != nil {
		t.Fatal(err)
	}
}

func setupBase(t *testing.T, o options) {
	t.Helper()
	var index v1.ImageIndex = empty.Index
	for _, arch := range []string{"amd64", "arm64"} {
		data, err := archive(map[string][]byte{"usr/bin/git": fixtureGit(arch)}, true)
		if err != nil {
			t.Fatal(err)
		}
		layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }, tarball.WithMediaType(types.OCILayer))
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
		config.OS, config.Architecture = "linux", arch
		image, err = mutate.ConfigFile(image, config)
		if err != nil {
			t.Fatal(err)
		}
		image = mutate.ConfigMediaType(mutate.MediaType(image, types.OCIManifestSchema1), types.OCIConfigJSON)
		index = mutate.AppendManifests(index, mutate.IndexAddendum{Add: image, Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: arch}}})
	}
	if _, err := layout.Write(o.base, index); err != nil {
		t.Fatal(err)
	}
	hash, err := index.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(o.pin, []byte(hash.String()+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestBaseRefusesUnlistedGoBinary(t *testing.T) {
	o := setup(t)
	index, err := checkedBase(o)
	if err != nil {
		t.Fatal(err)
	}
	image, err := baseImage(index, "amd64")
	if err != nil {
		t.Fatal(err)
	}
	binary, err := os.ReadFile(filepath.Join(filepath.Dir(o.dir), "binaries", "oberth-linux-amd64"))
	if err != nil {
		t.Fatal(err)
	}
	data, err := archive(map[string][]byte{"usr/local/bin/unlisted": binary}, true)
	if err != nil {
		t.Fatal(err)
	}
	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }, tarball.WithMediaType(types.OCILayer))
	if err != nil {
		t.Fatal(err)
	}
	image, err = mutate.AppendLayers(image, layer)
	if err != nil {
		t.Fatal(err)
	}
	pin, err := fixturePin(staticgit.AMD64)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := baseFiles(image, pin); err == nil {
		t.Fatal("base with extra Go binary accepted")
	}
}
func TestBaseRefusesNonOCIMediaTypes(t *testing.T) {
	o := setup(t)
	index, err := checkedBase(o)
	if err != nil {
		t.Fatal(err)
	}
	image, err := baseImage(index, "amd64")
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []v1.Image{mutate.MediaType(image, types.DockerManifestSchema2), mutate.ConfigMediaType(image, types.DockerConfigJSON)} {
		if ociImage(candidate) == nil {
			t.Fatal("non-OCI image accepted")
		}
	}
	data, err := archive(map[string][]byte{"file": []byte("data")}, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, media := range []types.MediaType{types.DockerLayer, types.OCIUncompressedLayer} {
		layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }, tarball.WithMediaType(media))
		if err != nil {
			t.Fatal(err)
		}
		candidate, err := mutate.AppendLayers(image, layer)
		if err != nil {
			t.Fatal(err)
		}
		if ociImage(candidate) == nil {
			t.Fatal("unsupported layer in OCI image accepted", media)
		}
	}
}
func TestChartStampPreservesValuesAndHonoursIgnore(t *testing.T) {
	o := setup(t)
	original := []byte("# Owner documentation\nratio: 1.10\nempty:\nimageDigest: unset\n# More documentation\nsourcePluginImageDigest: unset\n")
	for name, body := range map[string][]byte{"values.yaml": original, ".helmignore": []byte("ignored/\n*.secret\n"), "keep.txt": []byte("kept"), "file.secret": []byte("omit")} {
		if err := os.WriteFile(filepath.Join(o.chart, name), body, 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(o.chart, "ignored"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(o.chart, "ignored", "file"), []byte("omit"), 0644); err != nil {
		t.Fatal(err)
	}
	images := map[string]string{"oberth": "sha256:" + strings.Repeat("a", 64), "oberth-source-plugin": "sha256:" + strings.Repeat("b", 64)}
	if err := chart(o, images); err != nil {
		t.Fatal(err)
	}
	files := readChartFixture(t, o)
	expected := bytes.Replace(original, []byte("imageDigest: unset\n"), []byte("imageDigest: "+images["oberth"]+"\n"), 1)
	expected = bytes.Replace(expected, []byte("sourcePluginImageDigest: unset\n"), []byte("sourcePluginImageDigest: "+images["oberth-source-plugin"]+"\n"), 1)
	if !bytes.Equal(files["oberth/values.yaml"], expected) {
		t.Fatalf("values rewritten: %s", files["oberth/values.yaml"])
	}
	if len(files["oberth/keep.txt"]) == 0 || files["oberth/file.secret"] != nil || files["oberth/ignored/file"] != nil {
		t.Fatal("helmignore contract failed")
	}
}

func readChartFixture(t *testing.T, o options) map[string][]byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(o.dir, "oberth.tgz"))
	if err != nil {
		t.Fatal(err)
	}
	reader, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	files, err := readArchive(reader)
	closeErr := reader.Close()
	if err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}
	return files
}

func TestChartStampRefusesMissingOrRepeatedPlaceholders(t *testing.T) {
	images := map[string]string{"oberth": "sha256:" + strings.Repeat("a", 64), "oberth-source-plugin": "sha256:" + strings.Repeat("b", 64)}
	for _, values := range []string{"imageDigest: unset\n", "imageDigest: unset\nimageDigest: unset\nsourcePluginImageDigest: unset\n", " imageDigest: unset\nsourcePluginImageDigest: unset\n"} {
		if _, err := stampValues([]byte(values), images); !errors.Is(err, chartStamp) {
			t.Fatal("invalid placeholders accepted", err)
		}
	}
}

func digest(body []byte) string { return fmt.Sprintf("sha256:%x", sha256.Sum256(body)) }
