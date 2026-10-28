package argojob

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func newReleaseChartDigestFixture(test *testing.T) *releaseRuntimeFixture {
	test.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		test.Skip("Helm is required for release chart packaging tests")
	}
	fixture := newReleaseRuntimeFixture(test)
	fixture.write("require-signature", "yes")
	chart := filepath.Join(fixture.root, "charts", "oberth")
	if err := os.CopyFS(chart, os.DirFS(filepath.Join(repositoryRoot(test), "charts", "oberth"))); err != nil {
		test.Fatal(err)
	}
	fixture.env = append(fixture.env, "FIXTURE_HELM="+helm)
	fixture.tool("helm", `printf 'helm:%s\n' "$1" >>"$FIXTURE_TRACE"
if [ "$1" = package ] && [ -f "$FIXTURE_ROOT/package-stale-chart" ]; then
 shift 2
 set -- package "$FIXTURE_ROOT/charts/oberth" "$@"
fi
exec "$FIXTURE_HELM" "$@"`)
	fixture.tool("oberth-release-support", `case "$1" in
 normalize-tree) find "$2" -exec touch -t 202609270000.00 {} +;;
 registry-token) printf fixture-restricted-access-token >"$4";;
 *) exit 90;;
esac`)
	fixture.tool("oberth-release-image", `case "$1" in
 publish)
  printf 'image:publish\n' >>"$FIXTURE_TRACE"
  printf 'europe-west4-docker.pkg.dev/skipopsmain/oberth/oberth@sha256:%064d\n' 0 | tr 0 c >"$FIXTURE_CANDIDATE/server-image.txt";;
 verify) printf 'image:verify\n' >>"$FIXTURE_TRACE";;
 static-sbom)
  test "$#" -eq 3 || { test "$#" -eq 4 && test "$4" = control; }
  case "$2" in amd64|arm64) :;; *) exit 90;; esac
  printf 'image:static-sbom:%s:%s\n' "$2" "${4:-actual}" >>"$FIXTURE_TRACE"
  printf 'fixture %s %s SBOM\n' "$2" "${4:-actual}" >"$3";;
 *) exit 90;;
esac`)
	fixture.tool("trivy", `printf 'trivy:scan\n' >>"$FIXTURE_TRACE"`)
	return fixture
}

func TestReleaseChartPackagesPublishedImageDigest(test *testing.T) {
	fixture := newReleaseChartDigestFixture(test)
	valuesPath := filepath.Join(fixture.root, "charts", "oberth", "values.yaml")
	original, err := os.ReadFile(valuesPath)
	if err != nil {
		test.Fatal(err)
	}
	for _, action := range []string{"publish-images", "package-chart"} {
		if output, err := fixture.run(action, action == "publish-images"); err != nil {
			test.Fatalf("%s: %v\n%s", action, err, output)
		}
	}
	requireReleaseChartScanOrder(test, fixture.log(), true)
	published, err := os.ReadFile(filepath.Join(fixture.candidate, "server-image.txt"))
	if err != nil {
		test.Fatal(err)
	}
	helm, err := exec.LookPath("helm")
	if err != nil {
		test.Fatal(err)
	}
	archive := filepath.Join(fixture.candidate, "oberth-1.2.3.tgz")
	command := exec.CommandContext(test.Context(), helm, "show", "values", archive)
	packaged, err := command.CombinedOutput()
	if err != nil {
		test.Fatalf("read packaged defaults: %v\n%s", err, packaged)
	}
	var values struct {
		Image struct {
			Ref string `yaml:"ref"`
		} `yaml:"image"`
	}
	if err := yaml.Unmarshal(packaged, &values); err != nil {
		test.Fatal(err)
	}
	if values.Image.Ref != strings.TrimSpace(string(published)) {
		test.Fatalf("packaged image.ref = %q, published %q", values.Image.Ref, published)
	}
	command = exec.CommandContext(test.Context(), helm, "template", "oberth", archive, "--namespace", "oberth")
	rendered, err := command.CombinedOutput()
	if err != nil || !bytes.Contains(rendered, bytes.TrimSpace(published)) {
		test.Fatalf("packaged chart does not render the published image: %v\n%s", err, rendered)
	}
	after, err := os.ReadFile(valuesPath)
	if err != nil {
		test.Fatal(err)
	}
	if !bytes.Equal(original, after) {
		test.Fatal("packaging modified source chart values")
	}
}

func TestReleaseChartRejectsPackagedSeedDigest(test *testing.T) {
	fixture := newReleaseChartDigestFixture(test)
	archive := filepath.Join(fixture.candidate, "oberth-1.2.3.tgz")
	original, err := os.ReadFile(archive)
	if err != nil {
		test.Fatal(err)
	}
	fixture.write("package-stale-chart", "")
	output, err := fixture.run("package-chart", false)
	if err == nil || !strings.Contains(output, "packaged chart does not contain the published server digest") {
		test.Fatalf("stale packaged image accepted: %v\n%s", err, output)
	}
	after, err := os.ReadFile(archive)
	if err != nil {
		test.Fatal(err)
	}
	if !bytes.Equal(original, after) {
		test.Fatal("stale archive replaced the release chart")
	}
}

// This is the actual release.sh entrypoint with real Helm and synthetic external
// tools. No scanner download or registry/signing credential is used by the test.
func requireReleaseChartScanOrder(test *testing.T, trace string, signed bool) {
	test.Helper()
	steps := []string{"image:publish", "image:verify", "image:static-sbom:amd64:actual", "image:static-sbom:arm64:actual", "image:static-sbom:amd64:control", "static-c-scan"}
	if signed {
		steps = append(steps, "trivy:scan", "cosign:sign:keyenv=", "image:verify", "helm:package")
	}
	remaining := trace
	for _, step := range steps {
		marker := step + "\n"
		index := strings.Index(remaining, marker)
		if index < 0 {
			test.Fatalf("release omitted or reordered %s: %s", step, trace)
		}
		remaining = remaining[index+len(marker):]
	}
}

func TestReleaseChartScanFailurePreventsSigning(test *testing.T) {
	fixture := newReleaseChartDigestFixture(test)
	fixture.staticGitScan("exit 25")
	output, err := fixture.run("publish-images", true)
	if err == nil {
		test.Fatalf("injected scan failure accepted: %s", output)
	}
	requireReleaseChartScanOrder(test, fixture.log(), false)
	if strings.Contains(fixture.log(), "cosign:sign:") || strings.Contains(fixture.log(), "helm:package") {
		test.Fatalf("scan failure reached signing or packaging: %s", fixture.log())
	}
}
