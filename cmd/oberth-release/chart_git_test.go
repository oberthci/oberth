package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"helm.sh/helm/v3/pkg/chart/loader"
)

func TestReleaseImageShipsChartGitBinary(t *testing.T) {
	chart, err := loader.LoadDir("../../charts/oberth")
	if err != nil {
		t.Fatal(err)
	}
	runtime := chart.Values["server"].(map[string]any)["runtime"].(map[string]any)
	gitBinary := runtime["git_binary"].(string)
	if !filepath.IsAbs(gitBinary) {
		t.Fatal("chart Git path must be absolute", gitBinary)
	}
	o := setup(t)
	index, err := checkedBase(o)
	if err != nil {
		t.Fatal(err)
	}
	for _, arch := range []string{"amd64", "arm64"} {
		t.Run(arch, func(t *testing.T) {
			name := "oberth-linux-" + arch
			copyFile(t, filepath.Join(filepath.Dir(o.dir), "binaries", name), filepath.Join(o.dir, name))
			image, err := platformImage(o, index, "oberth", arch)
			if err != nil {
				t.Fatal(err)
			}
			reader := mutate.Extract(image)
			files, err := readArchive(reader)
			closeErr := reader.Close()
			if err != nil || closeErr != nil {
				t.Fatal(err, closeErr)
			}
			shipped := files[strings.TrimPrefix(gitBinary, "/")]
			if !bytes.Equal(shipped, fixtureGit(arch)) {
				t.Fatal("release image does not ship Git at the configured chart path", gitBinary)
			}
		})
	}
}
