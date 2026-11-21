//go:build base_image_qualification

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/oberthci/oberth/internal/staticgit"
)

// Package the native-runtime payload using the product's actual image function.
func TestPackageBaseQualificationImages(t *testing.T) {
	root := os.Getenv("OBERTH_BASE_QUALIFICATION")
	if !filepath.IsAbs(root) {
		t.Fatal("set OBERTH_BASE_QUALIFICATION to the qualification workspace")
	}
	options := options{base: filepath.Join(root, "base-image"), pin: filepath.Join(root, "candidate.digest"), dir: filepath.Join(root, "qualification"), git: staticgit.Qualified}
	index, err := checkedBase(options)
	if err != nil {
		t.Fatal(err)
	}
	for _, architecture := range []string{"amd64", "arm64"} {
		verifyQualificationBuild(t, filepath.Join(root, "artifacts", architecture), architecture)
		image, err := platformImage(options, index, "oberth", architecture)
		if err != nil {
			t.Fatal(err)
		}
		reference, err := name.NewTag("oberth-base-qualification:" + architecture)
		if err != nil {
			t.Fatal(err)
		}
		if err = tarball.WriteToFile(filepath.Join(root, "qualification", architecture+".tar"), reference, image); err != nil {
			t.Fatal(err)
		}
	}
}

func verifyQualificationBuild(t *testing.T, directory, architecture string) {
	t.Helper()
	record, err := staticgit.Qualified(staticgit.Architecture(architecture))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{}
	for _, name := range []string{"git", "musl.a", "zlib.a", "installed-packages.txt"} {
		files[name], err = os.ReadFile(filepath.Join(directory, name))
		if err != nil {
			t.Fatal(err)
		}
	}
	if err = record.VerifyBuild(files["git"], files["musl.a"], files["zlib.a"], string(files["installed-packages.txt"])); err != nil {
		t.Fatal(err)
	}
}
