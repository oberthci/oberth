package installer_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/releaseverify"
	"github.com/oberthci/oberth/internal/releaseverify/verifytest"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
)

type releaseData struct {
	files   fstest.MapFS
	version string
}

func releaseFixture(t *testing.T) releaseData {
	t.Helper()
	return releaseFor(t, "v0.1.0", 1)
}

type releaseKey struct {
	version string
	schema  int
}

//nolint:gochecknoglobals // Tests mutate only owned copies of cached signed release bytes.
var releaseFixtures struct {
	sync.Mutex
	values map[releaseKey]releaseData
}

func releaseFor(t *testing.T, version string, schema int) releaseData {
	t.Helper()
	releaseFixtures.Lock()
	defer releaseFixtures.Unlock()
	key := releaseKey{version, schema}
	f, ok := releaseFixtures.values[key]
	if !ok {
		f = buildReleaseFixture(t, version, schema)
		if releaseFixtures.values == nil {
			releaseFixtures.values = map[releaseKey]releaseData{}
		}
		releaseFixtures.values[key] = f
	}
	files := make(fstest.MapFS, len(f.files))
	for name, file := range f.files {
		copy := *file
		copy.Data = bytes.Clone(file.Data)
		files[name] = &copy
	}
	return releaseData{files: files, version: f.version}
}

func buildReleaseFixture(t *testing.T, version string, schema int) releaseData {
	t.Helper()
	ch, err := loader.Load(chartPath())
	if err != nil {
		t.Fatal(err)
	}
	files := make([]*loader.BufferedFile, 0, len(ch.Raw))
	for _, file := range ch.Raw {
		body := bytes.Clone(file.Data)
		switch file.Name {
		case "Chart.yaml":
			body = bytes.Replace(body, []byte("version: 0.0.0\n"), []byte("version: "+strings.TrimPrefix(version, "v")+"\n"), 1)
			body = bytes.Replace(body, []byte("appVersion: unset\n"), []byte("appVersion: "+version+"\n"), 1)
		case "values.yaml":
			body = bytes.Replace(body, []byte("imageDigest: unset\n"), []byte("imageDigest: sha256:"+strings.Repeat("b", 64)+"\n"), 1)
			body = bytes.Replace(body, []byte("sourcePluginImageDigest: unset\n"), []byte("sourcePluginImageDigest: sha256:"+strings.Repeat("c", 64)+"\n"), 1)
			body = bytes.Replace(body, fmt.Appendf(nil, "targetSchema: %v\n", ch.Values["targetSchema"]), fmt.Appendf(nil, "targetSchema: %d\n", schema), 1)
		}
		files = append(files, &loader.BufferedFile{Name: file.Name, Data: body})
	}
	ch, err = loader.LoadFiles(files)
	if err != nil {
		t.Fatal(err)
	}
	path, err := chartutil.Save(ch, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	archive, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	record, err := json.Marshal(releaseverify.Record{Version: version, Commit: strings.Repeat("a", 40), Images: map[string]string{"oberth": "sha256:" + strings.Repeat("b", 64), "oberth-source-plugin": "sha256:" + strings.Repeat("c", 64)}, Base: "sha256:" + strings.Repeat("d", 64)})
	if err != nil {
		t.Fatal(err)
	}
	f := releaseData{version: version, files: fstest.MapFS{"oberth.tgz": {Data: archive}, "release.json": {Data: record}}}
	signRelease(t, f.files)
	return f
}
func signRelease(t *testing.T, files fstest.MapFS) {
	t.Helper()
	sums := fmt.Appendf(nil, "%x  oberth.tgz\n%x  release.json\n", sha256.Sum256(files["oberth.tgz"].Data), sha256.Sum256(files["release.json"].Data))
	bundle, err := verifytest.Sign(sums)
	if err != nil {
		t.Fatal(err)
	}
	files["SHA256SUMS"] = &fstest.MapFile{Data: sums}
	files["SHA256SUMS.sigstore.json"] = &fstest.MapFile{Data: bundle}
}
func loadFixture(t *testing.T, f releaseData) installer.Release {
	t.Helper()
	r, err := installer.LoadRelease(t.Context(), f.files, f.version)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
