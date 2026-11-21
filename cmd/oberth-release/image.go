package main

import (
	"bytes"
	"debug/buildinfo"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/match"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/partial"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/google/go-containerregistry/pkg/v1/validate"
	"github.com/oberthci/oberth/internal/staticgit"
)

func checkedBase(o options) (v1.ImageIndex, error) {
	index, err := layout.ImageIndexFromPath(o.base)
	if err != nil {
		return nil, err
	}
	hash, err := index.Digest()
	if err != nil {
		return nil, err
	}
	pin, err := os.ReadFile(o.pin)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", basePin, err)
	}
	if strings.TrimSpace(string(pin)) != hash.String() {
		return nil, baseMismatch
	}
	if err := validate.Index(index); err != nil {
		return nil, baseMismatch
	}
	for _, arch := range []string{"amd64", "arm64"} {
		image, err := baseImage(index, arch)
		if err != nil {
			return nil, err
		}
		pin, err := o.git(staticgit.Architecture(arch))
		if err != nil {
			return nil, err
		}
		if err := ociImage(image); err != nil {
			return nil, err
		}
		if _, err := baseFiles(image, pin); err != nil {
			return nil, err
		}
	}
	return index, nil
}
func baseImage(index v1.ImageIndex, arch string) (v1.Image, error) {
	manifest, err := index.IndexManifest()
	if err != nil || len(manifest.Manifests) != 2 || manifest.MediaType != types.OCIImageIndex {
		return nil, baseMismatch
	}
	for _, descriptor := range manifest.Manifests {
		if descriptor.MediaType != types.OCIManifestSchema1 {
			return nil, baseMismatch
		}
	}
	images, err := partial.FindImages(index, match.Platforms(v1.Platform{OS: "linux", Architecture: arch}))
	if err != nil || len(images) != 1 {
		return nil, baseMismatch
	}
	return images[0], nil
}

func baseFiles(base v1.Image, pin staticgit.Record) (map[string][]byte, error) {
	reader := mutate.Extract(base)
	defer func() { _ = reader.Close() }()
	files, err := readArchive(reader)
	if err != nil {
		return nil, err
	}
	if err := pin.ValidateBinary(files["usr/bin/git"]); err != nil {
		return nil, baseMismatch
	}
	for _, body := range files {
		if _, err := buildinfo.Read(bytes.NewReader(body)); err == nil {
			return nil, binariesMismatch
		}
	}
	return files, nil
}
func images(o options, base v1.ImageIndex) (map[string]string, error) {
	digests := make(map[string]string)
	for _, name := range []string{"oberth", "oberth-source-plugin"} {
		var index v1.ImageIndex = empty.Index
		for _, arch := range []string{"amd64", "arm64"} {
			image, err := platformImage(o, base, name, arch)
			if err != nil {
				return nil, err
			}
			index = mutate.AppendManifests(index, mutate.IndexAddendum{Add: image, Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: arch}}})
		}
		index = mutate.IndexMediaType(index, types.OCIImageIndex)
		hash, err := index.Digest()
		if err != nil {
			return nil, err
		}
		digests[name] = hash.String()
		if err := saveIndex(o, name, index); err != nil {
			return nil, err
		}
	}
	return digests, nil
}
func platformImage(o options, index v1.ImageIndex, name, arch string) (v1.Image, error) {
	base, err := baseImage(index, arch)
	if err != nil {
		return nil, err
	}
	pin, err := o.git(staticgit.Architecture(arch))
	if err != nil {
		return nil, err
	}
	binary, err := os.ReadFile(filepath.Join(o.dir, name+"-linux-"+arch))
	if err != nil {
		return nil, inputs
	}
	layerBytes, err := archive(map[string][]byte{"usr/local/bin/" + name: binary}, true)
	if err != nil {
		return nil, err
	}
	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(layerBytes)), nil }, tarball.WithMediaType(types.OCILayer))
	if err != nil {
		return nil, err
	}
	image, err := mutate.AppendLayers(base, layer)
	if err != nil {
		return nil, err
	}
	config, err := image.ConfigFile()
	if err != nil {
		return nil, err
	}
	config.Config = v1.Config{Entrypoint: []string{"/usr/local/bin/" + name}, Env: []string{"PATH=/usr/local/bin:/usr/bin", "HOME=/data", "TMPDIR=/tmp"}, Labels: pin.Labels()}
	if name == "oberth" {
		config.Config.Cmd = []string{"serve"}
	}
	image, err = mutate.ConfigFile(image, config)
	if err != nil {
		return nil, err
	}
	return mutate.ConfigMediaType(mutate.MediaType(image, types.OCIManifestSchema1), types.OCIConfigJSON), nil
}
func saveIndex(o options, name string, index v1.ImageIndex) error {
	wrapper, err := layout.Write(filepath.Join(o.dir, name+".oci"), empty.Index)
	if err != nil {
		return err
	}
	return wrapper.AppendIndex(index, layout.WithAnnotations(map[string]string{"org.opencontainers.image.ref.name": o.version}))
}

func ociImage(image v1.Image) error {
	manifest, err := image.Manifest()
	if err != nil {
		return err
	}
	if manifest.MediaType != types.OCIManifestSchema1 || manifest.Config.MediaType != types.OCIConfigJSON {
		return baseMismatch
	}
	for _, layer := range manifest.Layers {
		if layer.MediaType != types.OCILayer {
			return baseMismatch
		}
	}
	return nil
}
