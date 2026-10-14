// KVM harness image building: apko-built runtime base + harness binary layer.
// Single-platform linux/amd64 only — no multi-platform index.
package releaseimage

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

const (
	RuntimeRepository = "europe-west4-docker.pkg.dev/skipopsmain/oberth/oberth-kvm-runtime"
	HarnessRepository = "europe-west4-docker.pkg.dev/skipopsmain/oberth/oberth-kvm-harness"
	harnessImagePath  = "usr/local/bin/oberth-kvm-harness"
)

// KVMPublished records the immutable digest references for KVM images.
type KVMPublished struct {
	Runtime string `json:"kvm_runtime"`
	Harness string `json:"kvm_harness"`
}

// LoadApkoImage reads a single-platform image from an apko-produced Docker-
// archive tarball. apko writes Docker-compatible tarballs with a manifest.json
// that go-containerregistry loads natively.
func LoadApkoImage(tarballPath string) (v1.Image, error) {
	// apko tags the image as "runtime:latest-amd64" in the Docker manifest.
	// tarball.ImageFromPath with a nil tag reads the first image entry.
	img, err := tarball.ImageFromPath(tarballPath, nil)
	if err != nil {
		return nil, fmt.Errorf("load apko Docker-archive tarball: %w", err)
	}
	cfg, err := img.ConfigFile()
	if err != nil {
		return nil, fmt.Errorf("read apko image config: %w", err)
	}
	if cfg.OS != "linux" || cfg.Architecture != "amd64" {
		return nil, fmt.Errorf("apko image platform is %s/%s, want linux/amd64", cfg.OS, cfg.Architecture)
	}
	return img, nil
}

// BuildHarnessImage creates the harness image by appending a single layer
// containing the harness binary onto the apko-built runtime base. The result
// is a single-platform linux/amd64 image with UID/GID 65534 and a fixed
// entrypoint, matching the Dockerfile contract.
func BuildHarnessImage(runtime v1.Image, binary []byte, created time.Time, version, revision string) (v1.Image, error) {
	if len(binary) == 0 {
		return nil, errors.New("harness binary is empty")
	}
	created = created.UTC().Truncate(time.Second)
	if created.IsZero() {
		return nil, errors.New("creation time is required")
	}

	// Create the layer tarball with exactly one file.
	var layerBuf bytes.Buffer
	tw := tar.NewWriter(&layerBuf)
	// Directory entries for the path hierarchy.
	for _, dir := range []string{"usr/", "usr/local/", "usr/local/bin/"} {
		if err := tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeDir, Name: dir, Mode: 0o755,
			ModTime: created, Format: tar.FormatPAX,
		}); err != nil {
			return nil, err
		}
	}
	if err := tw.WriteHeader(&tar.Header{
		Name: harnessImagePath, Mode: 0o555, Size: int64(len(binary)),
		Typeflag: tar.TypeReg, ModTime: created, Format: tar.FormatPAX,
	}); err != nil {
		return nil, err
	}
	if _, err := tw.Write(binary); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}

	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(layerBuf.Bytes())), nil
	})
	if err != nil {
		return nil, err
	}
	image, err := mutate.AppendLayers(runtime, layer)
	if err != nil {
		return nil, err
	}

	configFile, err := image.ConfigFile()
	if err != nil {
		return nil, err
	}
	configFile.Created = v1.Time{Time: created}
	configFile.Author = "Oberth"
	configFile.Config = v1.Config{
		User:       "65534:65534",
		Entrypoint: []string{"/usr/local/bin/oberth-kvm-harness"},
		Env:        []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"},
	}
	configFile.Config.Labels = map[string]string{
		"org.opencontainers.image.created":  created.Format(time.RFC3339),
		"org.opencontainers.image.revision": revision,
		"org.opencontainers.image.source":   "https://github.com/oberthci/oberth",
		"org.opencontainers.image.version":  version,
	}
	return mutate.ConfigFile(image, configFile)
}

// GARAuthenticator builds the GAR registry authenticator from a delegated
// access token. This is the single auth function to swap for WIF migration.
func GARAuthenticator(accessToken []byte) authn.Authenticator {
	return garAuthenticator(accessToken)
}

// PublishKVM builds the harness image from an apko runtime tarball and the
// local harness binary, then pushes both images to GAR.
func PublishKVM(ctx context.Context, config PublishConfig, runtimeTarball string) (KVMPublished, error) {
	if len(config.AccessToken) == 0 || config.Dist == "" {
		return KVMPublished{}, errors.New("GAR access token and distribution directory are required")
	}
	authenticator := GARAuthenticator(config.AccessToken)
	opts := append([]remote.Option{
		remote.WithContext(ctx), remote.WithAuth(authenticator),
		remote.WithUserAgent("oberth-release"),
	}, config.RemoteOpts...)

	// Load runtime from apko tarball.
	runtime, err := LoadApkoImage(runtimeTarball)
	if err != nil {
		return KVMPublished{}, err
	}

	// Push runtime image.
	runtimeRef, err := pushSingleImage(runtime, RuntimeRepository, config, opts)
	if err != nil {
		return KVMPublished{}, fmt.Errorf("push kvm runtime: %w", err)
	}

	// Read harness binary.
	binaryPath := filepath.Join(config.Dist, "oberth-kvm-harness")
	//nolint:gosec // path is constrained to the validated release dist directory and fixed name.
	binary, err := os.ReadFile(binaryPath)
	if err != nil {
		return KVMPublished{}, fmt.Errorf("read harness binary: %w", err)
	}

	// Build harness image.
	harness, err := BuildHarnessImage(runtime, binary, config.Created, config.Tag, config.SHA)
	if err != nil {
		return KVMPublished{}, err
	}

	// Push harness image.
	harnessRef, err := pushSingleImage(harness, HarnessRepository, config, opts)
	if err != nil {
		return KVMPublished{}, fmt.Errorf("push kvm harness: %w", err)
	}

	return KVMPublished{Runtime: runtimeRef, Harness: harnessRef}, nil
}

// pushSingleImage pushes a single-platform image (no index) to GAR with
// an idempotent tag and returns the immutable digest reference.
func pushSingleImage(img v1.Image, repository string, config PublishConfig, opts []remote.Option) (string, error) {
	digest, err := img.Digest()
	if err != nil {
		return "", err
	}
	tagName := fmt.Sprintf("%s:%s-%s", repository, strings.ReplaceAll(config.Tag, "+", "_"), config.SHA)
	tagRef, err := name.NewTag(tagName, name.StrictValidation)
	if err != nil {
		return "", err
	}

	existing, getErr := remote.Get(tagRef, opts...)
	if getErr == nil {
		if existing.Digest != digest {
			return "", fmt.Errorf("refusing to overwrite %s with digest %s; registry has %s", tagRef, digest, existing.Digest)
		}
	} else if !isNotFound(getErr) {
		return "", fmt.Errorf("inspect %s: %w", tagRef, getErr)
	} else if err := remote.Write(tagRef, img, opts...); err != nil {
		if _, readErr := remote.Get(tagRef, opts...); readErr != nil {
			return "", errors.Join(fmt.Errorf("push %s: %w", tagRef, err), readErr)
		}
	}

	readback, err := remote.Get(tagRef, opts...)
	if err != nil {
		return "", fmt.Errorf("read back %s: %w", tagRef, err)
	}
	if readback.Digest != digest {
		return "", fmt.Errorf("registry readback for %s is %s, want %s", tagRef, readback.Digest, digest)
	}
	return repository + "@" + digest.String(), nil
}

// VerifyKVM performs a complete authenticated read-back of the published
// KVM images and verifies the harness image contains the expected binary.
func VerifyKVM(ctx context.Context, config PublishConfig, published KVMPublished) error {
	if len(config.AccessToken) == 0 || config.Dist == "" || published.Runtime == "" || published.Harness == "" {
		return errors.New("GAR access token, dist, and both digest references are required")
	}
	authenticator := GARAuthenticator(config.AccessToken)
	opts := append([]remote.Option{
		remote.WithContext(ctx), remote.WithAuth(authenticator),
		remote.WithUserAgent("oberth-release"),
	}, config.RemoteOpts...)

	// Verify runtime reference is in the expected repository.
	runtimeDigest, err := name.NewDigest(published.Runtime, name.StrictValidation)
	if err != nil || runtimeDigest.Context().Name() != RuntimeRepository {
		return errors.New("kvm runtime reference is not an exact digest in its fixed repository")
	}
	runtimeDesc, err := remote.Get(runtimeDigest, opts...)
	if err != nil {
		return fmt.Errorf("read back kvm runtime: %w", err)
	}
	if runtimeDesc.Digest.String() != runtimeDigest.DigestStr() {
		return errors.New("kvm runtime digest changed during read-back")
	}

	// Verify harness reference.
	harnessDigestRef, err := name.NewDigest(published.Harness, name.StrictValidation)
	if err != nil || harnessDigestRef.Context().Name() != HarnessRepository {
		return errors.New("kvm harness reference is not an exact digest in its fixed repository")
	}
	harnessDesc, err := remote.Get(harnessDigestRef, opts...)
	if err != nil {
		return fmt.Errorf("read back kvm harness: %w", err)
	}
	if harnessDesc.Digest.String() != harnessDigestRef.DigestStr() {
		return errors.New("kvm harness digest changed during read-back")
	}

	// Verify the harness image contains the expected binary.
	harnessImage, err := harnessDesc.Image()
	if err != nil {
		return fmt.Errorf("read harness image: %w", err)
	}

	//nolint:gosec // path constrained to the validated release dist directory and fixed name.
	expectedBinary, err := os.ReadFile(filepath.Join(config.Dist, "oberth-kvm-harness"))
	if err != nil {
		return fmt.Errorf("read local harness binary for verification: %w", err)
	}

	return verifyKVMHarness(harnessImage, expectedBinary, config.Created, config.Tag, config.SHA)
}

func verifyKVMHarness(image v1.Image, binary []byte, created time.Time, version, revision string) error {
	configFile, err := image.ConfigFile()
	if err != nil {
		return err
	}
	if configFile.Architecture != "amd64" || configFile.OS != "linux" {
		return errors.New("kvm harness platform is not linux/amd64")
	}
	if configFile.Config.User != "65534:65534" {
		return errors.New("kvm harness user is not 65534:65534")
	}
	if len(configFile.Config.Entrypoint) != 1 || configFile.Config.Entrypoint[0] != "/usr/local/bin/oberth-kvm-harness" {
		return errors.New("kvm harness entrypoint is not the expected path")
	}

	// Extract and verify the binary from the last layer (the harness layer).
	layers, err := image.Layers()
	if err != nil {
		return err
	}
	if len(layers) < 2 {
		return errors.New("kvm harness image should have at least 2 layers (runtime + harness)")
	}
	lastLayer := layers[len(layers)-1]
	reader, err := lastLayer.Uncompressed()
	if err != nil {
		return err
	}
	defer func() { _ = reader.Close() }()
	tr := tar.NewReader(reader)
	foundBinary := false
	for {
		header, nextErr := tr.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			return nextErr
		}
		cleanName := strings.TrimPrefix(header.Name, "./")
		if cleanName == harnessImagePath {
			if foundBinary {
				return errors.New("duplicate harness binary in image")
			}
			if header.Mode&0o111 == 0 {
				return errors.New("harness binary is not executable")
			}
			body, readErr := io.ReadAll(io.LimitReader(tr, int64(len(binary))+1))
			if readErr != nil || !bytes.Equal(body, binary) {
				return errors.New("harness binary bytes differ from the source build")
			}
			foundBinary = true
		}
	}
	if !foundBinary {
		return errors.New("harness binary not found in image")
	}
	return nil
}

// WriteKVMReferences writes the published KVM image references to the
// release output directory for consumption by release-verify and release.json.
func WriteKVMReferences(dist string, published KVMPublished) error {
	for _, entry := range []struct {
		file, ref string
	}{
		{"kvm-runtime-image.txt", published.Runtime},
		{"kvm-harness-image.txt", published.Harness},
	} {
		path := filepath.Join(dist, entry.file)
		//nolint:gosec // path is a fixed filename beneath the explicit release dist directory.
		if err := os.WriteFile(path, []byte(entry.ref+"\n"), 0o600); err != nil {
			return err
		}
	}
	body, err := json.Marshal(published)
	if err != nil {
		return err
	}
	//nolint:gosec // path is a fixed filename beneath the explicit release dist directory.
	return os.WriteFile(filepath.Join(dist, "kvm-images.json"), append(body, '\n'), 0o600)
}
