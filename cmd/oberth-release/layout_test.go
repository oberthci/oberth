package main

import (
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"path/filepath"
)

func withImageIndex(o options, name string, consume func(v1.ImageIndex) error) error {
	wrapper, err := layout.FromPath(filepath.Join(o.dir, name+".oci"))
	if err != nil {
		return err
	}
	manifest, err := wrapper.ImageIndex()
	if err != nil {
		return err
	}
	descriptors, err := manifest.IndexManifest()
	if err != nil {
		return err
	}
	if len(descriptors.Manifests) != 1 {
		return binariesMismatch
	}
	index, err := manifest.ImageIndex(descriptors.Manifests[0].Digest)
	if err != nil {
		return err
	}
	return consume(index)
}
