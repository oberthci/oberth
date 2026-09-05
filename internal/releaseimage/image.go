// Package releaseimage assembles clean Oberth release images from reviewed,
// digest-pinned runtime substrates without a Docker daemon.
package releaseimage

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

const (
	ServerRepository = "europe-west4-docker.pkg.dev/skipopsmain/cloudtaser/cloudtaser-oberth"

	// The server substrate is used only as a reviewed package base. Repack
	// removes its Oberth executable and flattens the remaining filesystem into
	// a new image, so no prior Oberth code or image ancestry reaches the
	// release. The digest pins a multi-arch OCI index (linux/amd64 +
	// linux/arm64) built from this repository's Dockerfile; Publish pulls each
	// platform child through the index by digest.
	//
	// There is no runner image: Jobs run repository-declared standard golang
	// images executing `go -C .oberth run .`.
	//
	// Substrate 2026-09-06 (tag substrate-libcurl8220-20260906): rebuilt from
	// the Dockerfile for the 8-HIGH libcurl CVE set (CVE-2026-11352/-11586/
	// -12064/-80256/-8286/-8458/-8925/-8927/-9547) that failed the v0.13.32
	// release gate. Reviewed content delta vs the previous substrate
	// (6684009e...), identical on both platforms: libcurl 8.20.0-r0 ->
	// 8.22.0-r0 (the CVE fix, exact-pinned in the Dockerfile), libexpat
	// 2.8.3-r0 -> 2.8.4-r0 and pcre2 10.47-r0 -> 10.48-r0 (git dependencies,
	// repo patch level); 36 packages before and after, nothing added or
	// removed; /usr/local/bin/oberth present; exactly two platform children,
	// no attestation manifests.
	ServerSubstrate = ServerRepository + "@sha256:13dccb32a41bb8f67ae50cc6b0a59660ae6e161524a5e865d32880b073028c06"
)

// Kind selects a release image contract. Only the server image remains.
type Kind string

const (
	Server Kind = "server"
)

// PublishConfig binds an image publication to an exact source revision.
type PublishConfig struct {
	Tag        string
	SHA        string
	Created    time.Time
	KeyJSON    []byte
	Dist       string
	RemoteOpts []remote.Option
}

// Published records the immutable digest reference for the release image.
type Published struct {
	Server string
}

type imageContract struct {
	repository string
	substrate  string
	binaryName string
	imagePath  string
	platforms  []string
	required   []string
	forbidden  []string
	config     v1.Config
}

func contract(kind Kind) (imageContract, error) {
	switch kind {
	case Server:
		return imageContract{
			repository: ServerRepository,
			substrate:  ServerSubstrate,
			binaryName: "oberth",
			imagePath:  "usr/local/bin/oberth",
			platforms:  []string{"amd64", "arm64"},
			required: []string{
				"etc/ssl/certs/ca-certificates.crt", "usr/bin/git", "usr/bin/ssh",
			},
			config: v1.Config{
				User: "65534:65534", Env: []string{"HOME=/tmp", "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"},
				Entrypoint: []string{"/usr/local/bin/oberth"}, Cmd: []string{"serve"}, WorkingDir: "/tmp",
			},
		}, nil
	default:
		return imageContract{}, fmt.Errorf("unknown release image kind %q", kind)
	}
}

// Repack flattens a reviewed substrate, removes its prior executable, and adds
// exactly the newly built executable in a canonical release layer.
func Repack(base v1.Image, kind Kind, architecture string, binary []byte, created time.Time, version, revision string) (v1.Image, error) {
	definition, err := contract(kind)
	if err != nil {
		return nil, err
	}
	if !slices.Contains(definition.platforms, architecture) {
		return nil, fmt.Errorf("%s substrate does not support architecture %q", kind, architecture)
	}
	baseConfig, err := base.ConfigFile()
	if err != nil {
		return nil, fmt.Errorf("read %s substrate configuration: %w", kind, err)
	}
	if baseConfig.OS != "linux" || baseConfig.Architecture != architecture || baseConfig.Variant != "" {
		return nil, fmt.Errorf("%s substrate platform is %s/%s/%s, want linux/%s", kind, baseConfig.OS, baseConfig.Architecture, baseConfig.Variant, architecture)
	}
	if len(binary) == 0 {
		return nil, errors.New("release executable is empty")
	}
	created = created.UTC().Truncate(time.Second)
	if created.IsZero() {
		return nil, errors.New("release creation time is required")
	}

	var layerArchive bytes.Buffer
	target := tar.NewWriter(&layerArchive)
	extracted := mutate.Extract(base)
	defer func() { _ = extracted.Close() }()
	source := tar.NewReader(extracted)
	foundOldExecutable := false
	foundRequired := make(map[string]bool, len(definition.required))
	for {
		header, nextErr := source.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			return nil, fmt.Errorf("extract %s substrate: %w", kind, nextErr)
		}
		cleanName, cleanErr := cleanTarName(header.Name)
		if cleanErr != nil {
			return nil, fmt.Errorf("%s substrate: %w", kind, cleanErr)
		}
		if cleanName == definition.imagePath {
			foundOldExecutable = true
			continue
		}
		if forbiddenPath(cleanName, definition.forbidden) {
			return nil, fmt.Errorf("%s substrate contains forbidden repository dependency %s", kind, cleanName)
		}
		if slices.Contains(definition.required, cleanName) {
			foundRequired[cleanName] = true
		}
		canonical := *header
		canonical.Name = cleanName
		canonical.Uname = ""
		canonical.Gname = ""
		canonical.ModTime = created
		canonical.AccessTime = time.Time{}
		canonical.ChangeTime = time.Time{}
		canonical.PAXRecords = nil
		canonical.Xattrs = nil //nolint:staticcheck // Clearing the deprecated input field prevents substrate xattrs from leaking into the canonical layer.
		if err := target.WriteHeader(&canonical); err != nil {
			return nil, fmt.Errorf("write %s substrate entry %s: %w", kind, cleanName, err)
		}
		if canonical.Size > 0 {
			if _, err := io.CopyN(target, source, canonical.Size); err != nil {
				return nil, fmt.Errorf("copy %s substrate entry %s: %w", kind, cleanName, err)
			}
		}
	}
	if !foundOldExecutable {
		return nil, fmt.Errorf("%s substrate did not contain the executable that must be removed", kind)
	}
	for _, required := range definition.required {
		if !foundRequired[required] {
			return nil, fmt.Errorf("%s substrate lacks required bootstrap path %s", kind, required)
		}
	}
	if err := target.WriteHeader(&tar.Header{
		Name: definition.imagePath, Mode: 0o755, Size: int64(len(binary)), Typeflag: tar.TypeReg,
		Uid: 0, Gid: 0, ModTime: created, Format: tar.FormatPAX,
	}); err != nil {
		return nil, err
	}
	if _, err := target.Write(binary); err != nil {
		return nil, err
	}
	if err := target.Close(); err != nil {
		return nil, err
	}
	layerBytes := slices.Clone(layerArchive.Bytes())
	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(layerBytes)), nil
	})
	if err != nil {
		return nil, err
	}
	image, err := mutate.AppendLayers(empty.Image, layer)
	if err != nil {
		return nil, err
	}
	configFile, err := image.ConfigFile()
	if err != nil {
		return nil, err
	}
	configFile.Architecture = architecture
	configFile.OS = "linux"
	configFile.Created = v1.Time{Time: created}
	configFile.Author = "Oberth"
	configFile.Config = definition.config
	configFile.Config.Labels = map[string]string{
		"org.opencontainers.image.created":  created.Format(time.RFC3339),
		"org.opencontainers.image.revision": revision,
		"org.opencontainers.image.source":   "https://github.com/oberthci/oberth",
		"org.opencontainers.image.version":  version,
	}
	configFile.History = []v1.History{{
		Created: v1.Time{Time: created}, CreatedBy: "oberth repository release", Comment: "clean flattened substrate plus exact source-built executable",
	}}
	return mutate.ConfigFile(image, configFile)
}

func forbiddenPath(name string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if wildcardPrefix, wildcard := strings.CutSuffix(prefix, "*"); wildcard {
			if wildcardPrefix != "" && strings.HasPrefix(name, wildcardPrefix) {
				return true
			}
			continue
		}
		if name == prefix || strings.HasPrefix(name, prefix+"/") {
			return true
		}
	}
	return false
}

func cleanTarName(name string) (string, error) {
	if strings.ContainsRune(name, '\x00') || strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("unsafe absolute or NUL tar path %q", name)
	}
	cleaned := path.Clean(strings.TrimPrefix(name, "./"))
	if cleaned == "." || cleaned == "" || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("unsafe tar path %q", name)
	}
	return cleaned, nil
}

// Publish builds and writes deterministic supported-platform indexes, then
// proves the registry readback digest for each exact source-specific tag.
func Publish(ctx context.Context, config PublishConfig) (Published, error) {
	if len(config.KeyJSON) == 0 || config.Dist == "" {
		return Published{}, errors.New("GAR key and distribution directory are required")
	}
	authenticator := garAuthenticator(config.KeyJSON)
	remoteOptions := append([]remote.Option{remote.WithContext(ctx), remote.WithAuth(authenticator), remote.WithUserAgent("oberth-release")}, config.RemoteOpts...)
	result := Published{}
	for _, kind := range []Kind{Server} {
		definition, _ := contract(kind)
		baseReference, err := name.ParseReference(definition.substrate, name.StrictValidation)
		if err != nil {
			return Published{}, err
		}
		var index v1.ImageIndex = empty.Index
		for _, architecture := range definition.platforms {
			binaryPath := filepath.Join(config.Dist, definition.binaryName+"-linux-"+architecture)
			// #nosec G304 -- binaryPath is constrained to the validated release distribution directory and fixed executable name.
			binary, err := os.ReadFile(binaryPath)
			if err != nil {
				return Published{}, fmt.Errorf("read %s: %w", binaryPath, err)
			}
			base, err := remote.Image(baseReference, append(remoteOptions, remote.WithPlatform(v1.Platform{OS: "linux", Architecture: architecture}))...)
			if err != nil {
				return Published{}, fmt.Errorf("read pinned %s substrate for %s: %w", kind, architecture, err)
			}
			image, err := Repack(base, kind, architecture, binary, config.Created, config.Tag, config.SHA)
			if err != nil {
				return Published{}, err
			}
			index = mutate.AppendManifests(index, mutate.IndexAddendum{
				Add:        image,
				Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: architecture}},
			})
		}
		digest, err := index.Digest()
		if err != nil {
			return Published{}, err
		}
		tagName := fmt.Sprintf("%s:%s-%s", definition.repository, strings.ReplaceAll(config.Tag, "+", "_"), config.SHA)
		tagReference, err := name.NewTag(tagName, name.StrictValidation)
		if err != nil {
			return Published{}, err
		}
		existing, getErr := remote.Get(tagReference, remoteOptions...)
		if getErr == nil {
			if existing.Digest != digest {
				return Published{}, fmt.Errorf("refusing to overwrite %s with digest %s; registry has %s", tagReference, digest, existing.Digest)
			}
		} else if !isNotFound(getErr) {
			return Published{}, fmt.Errorf("inspect %s: %w", tagReference, getErr)
		} else if err := remote.WriteIndex(tagReference, index, remoteOptions...); err != nil {
			// A lost response is ambiguous. Exact digest readback below decides.
			if _, readErr := remote.Get(tagReference, remoteOptions...); readErr != nil {
				return Published{}, errors.Join(fmt.Errorf("publish %s: %w", tagReference, err), readErr)
			}
		}
		readback, err := remote.Get(tagReference, remoteOptions...)
		if err != nil {
			return Published{}, fmt.Errorf("read back %s: %w", tagReference, err)
		}
		if readback.Digest != digest {
			return Published{}, fmt.Errorf("registry readback for %s is %s, want %s", tagReference, readback.Digest, digest)
		}
		digestReference := definition.repository + "@" + digest.String()
		if kind == Server {
			result.Server = digestReference
		}
	}
	return result, nil
}

// Verify performs a complete authenticated read-back of the published
// multi-platform image and binds every child filesystem to the exact local
// source-built executable. A manifest-only registry response is not evidence
// that the referenced blobs or runtime contract are intact.
func Verify(ctx context.Context, config PublishConfig, published Published) error {
	if len(config.KeyJSON) == 0 || config.Dist == "" || published.Server == "" {
		return errors.New("GAR key, distribution directory, and the digest reference are required")
	}
	authenticator := garAuthenticator(config.KeyJSON)
	remoteOptions := append([]remote.Option{remote.WithContext(ctx), remote.WithAuth(authenticator), remote.WithUserAgent("oberth-release")}, config.RemoteOpts...)
	for _, item := range []struct {
		kind      Kind
		reference string
	}{
		{Server, published.Server},
	} {
		definition, err := contract(item.kind)
		if err != nil {
			return err
		}
		reference, err := name.NewDigest(item.reference, name.StrictValidation)
		if err != nil || reference.Context().Name() != definition.repository {
			return fmt.Errorf("%s release reference is not an exact digest in its fixed repository", item.kind)
		}
		descriptor, err := remote.Get(reference, remoteOptions...)
		if err != nil {
			return fmt.Errorf("read back %s release index: %w", item.kind, err)
		}
		if descriptor.Digest.String() != reference.DigestStr() {
			return fmt.Errorf("%s release index digest changed during read-back", item.kind)
		}
		index, err := descriptor.ImageIndex()
		if err != nil {
			return fmt.Errorf("%s release reference is not an image index: %w", item.kind, err)
		}
		manifest, err := index.IndexManifest()
		if err != nil {
			return fmt.Errorf("read %s release index manifest: %w", item.kind, err)
		}
		if len(manifest.Manifests) != len(definition.platforms) {
			return fmt.Errorf("%s release index has %d children, want exactly %d", item.kind, len(manifest.Manifests), len(definition.platforms))
		}
		platformDigests := make(map[string]v1.Hash, len(definition.platforms))
		for _, child := range manifest.Manifests {
			if child.Platform == nil || child.Platform.OS != "linux" || child.Platform.Variant != "" ||
				!slices.Contains(definition.platforms, child.Platform.Architecture) {
				return fmt.Errorf("%s release index contains an unsupported platform", item.kind)
			}
			if _, duplicate := platformDigests[child.Platform.Architecture]; duplicate {
				return fmt.Errorf("%s release index repeats platform %s", item.kind, child.Platform.Architecture)
			}
			platformDigests[child.Platform.Architecture] = child.Digest
		}
		for _, architecture := range definition.platforms {
			expectedDigest, ok := platformDigests[architecture]
			if !ok {
				return fmt.Errorf("%s release index lacks linux/%s", item.kind, architecture)
			}
			platformOptions := append(slices.Clone(remoteOptions), remote.WithPlatform(v1.Platform{OS: "linux", Architecture: architecture}))
			image, err := remote.Image(reference, platformOptions...)
			if err != nil {
				return fmt.Errorf("read back %s linux/%s image: %w", item.kind, architecture, err)
			}
			actualDigest, err := image.Digest()
			if err != nil || actualDigest != expectedDigest {
				return fmt.Errorf("%s linux/%s child digest does not match its index descriptor", item.kind, architecture)
			}
			// #nosec G304 -- the path is constrained to the validated release distribution directory and fixed executable name.
			binary, err := os.ReadFile(filepath.Join(config.Dist, definition.binaryName+"-linux-"+architecture))
			if err != nil {
				return fmt.Errorf("read local %s linux/%s executable: %w", item.kind, architecture, err)
			}
			if err := verifyImage(image, definition, architecture, binary, config.Created, config.Tag, config.SHA); err != nil {
				return fmt.Errorf("verify %s linux/%s image: %w", item.kind, architecture, err)
			}
		}
	}
	return nil
}

func verifyImage(image v1.Image, definition imageContract, architecture string, binary []byte, created time.Time, version, revision string) error {
	layers, err := image.Layers()
	if err != nil {
		return err
	}
	if len(layers) != 1 {
		return fmt.Errorf("release image has %d layers, want one clean flattened layer", len(layers))
	}
	configFile, err := image.ConfigFile()
	if err != nil {
		return err
	}
	created = created.UTC().Truncate(time.Second)
	expectedConfig := definition.config
	expectedConfig.Labels = map[string]string{
		"org.opencontainers.image.created":  created.Format(time.RFC3339),
		"org.opencontainers.image.revision": revision,
		"org.opencontainers.image.source":   "https://github.com/oberthci/oberth",
		"org.opencontainers.image.version":  version,
	}
	if configFile.OS != "linux" || configFile.Architecture != architecture || !configFile.Created.Equal(created) ||
		!reflect.DeepEqual(configFile.Config, expectedConfig) || len(configFile.History) != 1 {
		return errors.New("release image configuration is not the canonical runtime contract")
	}
	reader := mutate.Extract(image)
	defer func() { _ = reader.Close() }()
	archive := tar.NewReader(reader)
	foundRequired := make(map[string]bool, len(definition.required))
	foundExecutable := false
	for {
		header, nextErr := archive.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			return nextErr
		}
		cleanName, err := cleanTarName(header.Name)
		if err != nil {
			return err
		}
		if slices.Contains(definition.required, cleanName) {
			foundRequired[cleanName] = true
		}
		if forbiddenPath(cleanName, definition.forbidden) {
			return fmt.Errorf("release image contains forbidden repository dependency %s", cleanName)
		}
		if cleanName != definition.imagePath {
			continue
		}
		if foundExecutable || !regularTarType(header.Typeflag) || header.Mode&0o111 == 0 {
			return errors.New("release executable entry is duplicate, non-regular, or non-executable")
		}
		body, err := io.ReadAll(io.LimitReader(archive, int64(len(binary))+1))
		if err != nil || !bytes.Equal(body, binary) {
			return errors.New("release executable bytes differ from the source build")
		}
		foundExecutable = true
	}
	if !foundExecutable {
		return errors.New("release executable is absent")
	}
	for _, required := range definition.required {
		if !foundRequired[required] {
			return fmt.Errorf("release image lacks required bootstrap path %s", required)
		}
	}
	return nil
}

func regularTarType(typeFlag byte) bool {
	return typeFlag == tar.TypeReg || typeFlag == 0
}

// garAuthenticator builds the GAR registry authenticator from the raw key
// JSON. Exactly one string copy is made here, at the last possible moment
// before the third-party API boundary.
//
// Accepted residual: the Go string inside the returned authn.Authenticator is
// immutable and cannot be zeroed (Go runtime/GC limitation);
// go-containerregistry additionally derives a base64 basic-auth form
// internally. The caller must zero the source []byte (keyJSON) after the
// authenticator's last use.
func garAuthenticator(keyJSON []byte) authn.Authenticator {
	return authn.FromConfig(authn.AuthConfig{
		Username: "_json_key",
		Password: string(keyJSON), // sole string conversion — see residual note above
	})
}

func isNotFound(err error) bool {
	var transportError *transport.Error
	return errors.As(err, &transportError) && transportError.StatusCode == 404
}
