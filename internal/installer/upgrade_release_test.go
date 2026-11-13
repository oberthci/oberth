package installer

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/releaseverify"
)

// testReleaseChart is the stand-in chart archive a test release signs.
const testReleaseChart = "verified chart archive"

func testSignBundle(t testing.TB, key *ecdsa.PrivateKey, payload []byte) []byte {
	t.Helper()
	sum := sha256.Sum256(payload)
	signature, err := ecdsa.SignASN1(rand.Reader, key, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	hint := sha256.Sum256(der)
	raw, err := json.Marshal(map[string]any{
		"mediaType":            "application/vnd.dev.sigstore.bundle.v0.3+json",
		"verificationMaterial": map[string]any{"publicKey": map[string]any{"hint": base64.StdEncoding.EncodeToString(hint[:])}},
		"messageSignature": map[string]any{
			"messageDigest": map[string]any{"algorithm": "SHA2_256", "digest": base64.StdEncoding.EncodeToString(sum[:])},
			"signature":     base64.StdEncoding.EncodeToString(signature),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// testReleaseObjects returns the published objects of a release signed by
// key, keyed by URL under the default origins.
func testReleaseObjects(t testing.TB, key *ecdsa.PrivateKey, version, imageRef string) map[string][]byte {
	t.Helper()
	tag := canonicalChartVersion(version)
	chart := []byte(testReleaseChart)
	sum := sha256.Sum256(chart)
	record, err := json.Marshal(map[string]any{
		"schemaVersion": 1, "component": "oberth", "version": tag,
		"source": map[string]any{"repository": releaseverify.SourceRepository, "sha": strings.Repeat("a", 40)},
		"images": map[string]any{"server": map[string]any{"ref": imageRef}},
		"chart":  map[string]any{"version": strings.TrimPrefix(tag, "v"), "digest": "sha256:" + hex.EncodeToString(sum[:])},
	})
	if err != nil {
		t.Fatal(err)
	}
	recordName, recordBundle, chartName, chartBundle := releaseverify.Names(tag)
	base := releaseverify.DefaultReleaseOrigin + "/oberth/" + tag + "/"
	return map[string][]byte{
		base + recordName:   record,
		base + recordBundle: testSignBundle(t, key, record),
		releaseverify.DefaultChartRepository + "/" + chartName:   chart,
		releaseverify.DefaultChartRepository + "/" + chartBundle: testSignBundle(t, key, chart),
	}
}

// withSignedRelease makes deps serve a signed release of version whose
// record pins imageRef, under a test key it trusts.
func withSignedRelease(t testing.TB, deps *Deps, version, imageRef string) {
	t.Helper()
	deps.ReleaseGetter, deps.releaseKey = signedReleaseServer(t, version, imageRef)
}

// signedReleaseServer serves a signed release of version whose record pins
// imageRef, and returns the test key that trusts it.
func signedReleaseServer(t testing.TB, version, imageRef string) (releaseverify.Getter, *ecdsa.PublicKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	objects := testReleaseObjects(t, key, version, imageRef)
	return func(_ context.Context, url string, _ int64) ([]byte, error) {
		if body, ok := objects[url]; ok {
			return body, nil
		}
		return nil, fmt.Errorf("fetch %s: HTTP 404", url)
	}, &key.PublicKey
}

var verifiedChartPathPattern = regexp.MustCompile(`\S*/oberth-release-[^/\s]+/(oberth-[^/\s]+\.tgz)`)

// normalizeVerifiedChart rewrites the private file a verified release chart
// was written to as "<verified oberth-X.tgz>", the name dry-run plans use.
func normalizeVerifiedChart(text string) string {
	return verifiedChartPathPattern.ReplaceAllString(text, "<verified $1>")
}

// isVerifiedChartArg reports whether a Helm chart argument is the private
// file the verified release chart was written to.
func isVerifiedChartArg(arg, version string) bool {
	_, _, chartName, _ := releaseverify.Names(canonicalChartVersion(version))
	if filepath.Base(arg) != chartName || !strings.Contains(arg, "oberth-release-") {
		return false
	}
	return true
}

func TestVerifyUpgradeReleaseFailsClosed(t *testing.T) {
	t.Parallel()
	const version = "0.21.0"
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	objects := testReleaseObjects(t, key, version, fakeChartImageRef)
	serve := func(objects map[string][]byte) releaseverify.Getter {
		return func(_ context.Context, url string, _ int64) ([]byte, error) {
			if body, ok := objects[url]; ok {
				return body, nil
			}
			return nil, errors.New("HTTP 404")
		}
	}
	deps := Deps{ReleaseGetter: serve(objects), releaseKey: &key.PublicKey}
	release, err := verifyUpgradeRelease(context.Background(), UpgradeConfig{BinaryVersion: version}, deps)
	if err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(release.chartPath)
	if err != nil || string(written) != testReleaseChart {
		t.Fatalf("verified chart file = %q, %v", written, err)
	}
	if info, _ := os.Stat(release.chartPath); info.Mode().Perm() != 0o600 {
		t.Fatalf("verified chart mode %v", info.Mode())
	}
	if err := release.requireRecordImage(fakeChartImageRef); err != nil {
		t.Fatal(err)
	}
	if err := release.requireRecordImage("registry.example/other@sha256:" + strings.Repeat("0", 64)); err == nil {
		t.Fatal("chart image differing from the signed record accepted")
	}
	release.cleanup()
	if _, err := os.Stat(release.chartPath); !os.IsNotExist(err) {
		t.Fatal("verified chart left behind")
	}

	// The compiled-in key does not trust the test key: no test-key release
	// can pass without the override.
	if _, err := verifyUpgradeRelease(context.Background(), UpgradeConfig{BinaryVersion: version}, Deps{ReleaseGetter: serve(objects)}); err == nil ||
		!strings.Contains(err.Error(), "refusing unverified release") {
		t.Fatalf("test-key release under the pinned key: %v", err)
	}
	for name, change := range map[string]func(map[string][]byte){
		"missing chart bundle": func(o map[string][]byte) {
			delete(o, releaseverify.DefaultChartRepository+"/oberth-0.21.0.tgz.sigstore.json")
		},
		"swapped chart": func(o map[string][]byte) {
			o[releaseverify.DefaultChartRepository+"/oberth-0.21.0.tgz"] = []byte("other chart")
		},
		"other version record": func(o map[string][]byte) {
			other := testReleaseObjects(t, key, "0.20.9", fakeChartImageRef)
			o[releaseverify.DefaultReleaseOrigin+"/oberth/v0.21.0/release.json"] = other[releaseverify.DefaultReleaseOrigin+"/oberth/v0.20.9/release.json"]
			o[releaseverify.DefaultReleaseOrigin+"/oberth/v0.21.0/release.json.sigstore.json"] = other[releaseverify.DefaultReleaseOrigin+"/oberth/v0.20.9/release.json.sigstore.json"]
		},
	} {
		changed := map[string][]byte{}
		for k, v := range objects {
			changed[k] = bytes.Clone(v)
		}
		change(changed)
		if _, err := verifyUpgradeRelease(context.Background(), UpgradeConfig{BinaryVersion: version}, Deps{ReleaseGetter: serve(changed), releaseKey: &key.PublicKey}); err == nil {
			t.Errorf("%s: verified", name)
		}
	}

	// --release-dir reads the same four objects from disk.
	dir := t.TempDir()
	for url, body := range objects {
		if err := os.WriteFile(filepath.Join(dir, url[strings.LastIndex(url, "/")+1:]), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	offline, err := verifyUpgradeRelease(context.Background(), UpgradeConfig{BinaryVersion: version, ReleaseDir: dir},
		Deps{ReleaseGetter: serve(nil), releaseKey: &key.PublicKey})
	if err != nil {
		t.Fatal(err)
	}
	offline.cleanup()
}

// An install rerun by a CLI older than the release verifies the CLI's own
// version first (the preflight) and then reconciles at the installed
// version: the chart Helm gets must be the installed version's, verified
// again, never the older chart the preflight verified.
func TestInstallReconcileOfANewerReleaseNeverDeploysThePreflightChart(t *testing.T) {
	t.Parallel()
	const cli, installed = "0.21.0", "0.21.1"
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	objects := testReleaseObjects(t, key, cli, fakeChartImageRef)
	for url, body := range testReleaseObjects(t, key, installed, fakeChartImageRef) {
		objects[url] = body
	}
	var upgradeArgs []string
	deps := Deps{
		Output: io.Discard,
		RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			if len(args) > 1 && args[0] == "get" && args[1] == "values" {
				return []byte(`{}`), nil
			}
			switch args[0] {
			case "list":
				return []byte(`[{"name":"oberth","namespace":"oberth","status":"deployed","chart":"oberth-` + installed + `"}]`), nil
			case "show":
				return []byte(fakeChartValuesYAML), nil
			case "upgrade":
				upgradeArgs = slices.Clone(args)
			}
			return nil, nil
		},
		ReleaseGetter: func(_ context.Context, url string, _ int64) ([]byte, error) {
			if body, ok := objects[url]; ok {
				return body, nil
			}
			return nil, fmt.Errorf("fetch %s: HTTP 404", url)
		},
		releaseKey: &key.PublicKey,
	}
	cfg := Config{ChartVersion: "v" + cli, InstallRekor: true}
	cleanup, err := preflightInstallRelease(context.Background(), &cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if !isVerifiedChartArg(cfg.verifiedChart, cli) {
		t.Fatalf("preflight chart = %q", cfg.verifiedChart)
	}
	rekor := RekorResult{ServiceAddress: "http://rekor-server.rekor.svc:80", LogPublicKeyPEM: "-----BEGIN PUBLIC KEY-----\nQUJD\n-----END PUBLIC KEY-----\n"}
	if _, err := InstallOberth(context.Background(), cfg, deps, OpenBaoResult{}, rekor); err != nil {
		t.Fatal(err)
	}
	if len(upgradeArgs) < 4 || !isVerifiedChartArg(upgradeArgs[3], installed) {
		t.Fatalf("helm upgrade deployed %v, want the verified chart of the installed version %s", upgradeArgs, installed)
	}
}
