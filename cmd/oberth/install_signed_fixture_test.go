package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/oberthci/oberth/internal/installer"
	chartfake "github.com/oberthci/oberth/internal/installer/fake"
	"github.com/oberthci/oberth/internal/kubeconfig"
	"github.com/oberthci/oberth/internal/releaseverify"
	"github.com/oberthci/oberth/internal/releaseverify/verifytest"
	"github.com/oberthci/oberth/internal/secretstore"
	storefake "github.com/oberthci/oberth/internal/secretstore/fake"
	"github.com/oberthci/oberth/internal/store"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	kversion "k8s.io/apimachinery/pkg/version"
	discoveryfake "k8s.io/client-go/discovery/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
)

func signedInstallRelease(t *testing.T, version string) (installer.Release, string) {
	t.Helper()
	chart, err := loader.Load("../../charts/oberth")
	if err != nil {
		t.Fatal(err)
	}
	chart.Metadata.Version = strings.TrimPrefix(version, "v")
	chart.Metadata.AppVersion = version
	chart.Values["imageDigest"] = "sha256:" + strings.Repeat("b", 64)
	chart.Values["sourcePluginImageDigest"] = "sha256:" + strings.Repeat("c", 64)
	chart.Values["targetSchema"] = store.Schema
	values, err := json.Marshal(chart.Values)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range chart.Raw {
		if file.Name == "values.yaml" {
			file.Data = values
		}
	}
	archive, err := chartutil.Save(chart, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	record, err := json.Marshal(releaseverify.Record{Version: version, Commit: strings.Repeat("a", 40), Base: "sha256:" + strings.Repeat("d", 64), Images: map[string]string{"oberth": "sha256:" + strings.Repeat("b", 64), "oberth-source-plugin": "sha256:" + strings.Repeat("c", 64)}})
	if err != nil {
		t.Fatal(err)
	}
	sums := fmt.Appendf(nil, "%x  oberth.tgz\n%x  release.json\n", sha256.Sum256(body), sha256.Sum256(record))
	signature, err := verifytest.Sign(sums)
	if err != nil {
		t.Fatal(err)
	}
	files := fstest.MapFS{"oberth.tgz": {Data: body}, "release.json": {Data: record}, "SHA256SUMS": {Data: sums}, "SHA256SUMS.sigstore.json": {Data: signature}}
	release, err := installer.LoadRelease(t.Context(), files, version)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for name, file := range files {
		if err = os.WriteFile(filepath.Join(dir, name), file.Data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return release, dir
}

func offlineInstallEngine(t *testing.T, base string) (*installer.Engine, *chartfake.Chart) {
	t.Helper()
	credentials, err := installer.OpenConfigDir(filepath.Join(base, "installed"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := credentials.Close(); err != nil {
			t.Error(err)
		}
	})
	kube := kubefake.NewClientset(&core.Namespace{ObjectMeta: meta.ObjectMeta{Name: "kube-system", UID: "install-command-cluster"}})
	chartfake.DryRunServices(kube)
	kube.Discovery().(*discoveryfake.FakeDiscovery).FakedServerVersion = &kversion.Info{GitVersion: "v1.30.0"}
	charts := &chartfake.Chart{}
	config := &rest.Config{Host: "https://127.0.0.1:6443"}
	config.WrapTransport = func(http.RoundTripper) http.RoundTripper {
		return testTransport(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"apiVersion":"argoproj.io/v1alpha1","kind":"WorkflowList","items":[]}`))}, nil
		})
	}
	engine := &installer.Engine{Definitions: &chartfake.Definitions{}, Cluster: kubeconfig.Target{Context: "chosen", Config: config, Client: kube}, Charts: charts, Credentials: credentials, Random: rand.Reader, Now: func() time.Time { return time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC) }, CheckEndpoint: func(context.Context, installer.Profile) error { return nil }}
	backend := &storefake.Store{}
	engine.OpenStore = func(context.Context, secretstore.Config) (*secretstore.Client, func(), error) {
		c, err := secretstore.New(secretstore.Connection{URL: "https://store.oberth.svc", Transport: backend})
		return c, func() { c.Close() }, err
	}
	return engine, charts
}
