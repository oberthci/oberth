package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"helm.sh/helm/v3/pkg/chart/loader"
	corev1 "k8s.io/api/core/v1"
)

func chartLoginAudience(t *testing.T) string {
	t.Helper()
	chart, err := loader.LoadDir("../../charts/oberth")
	if err != nil {
		t.Fatal(err)
	}
	store, ok := chart.Values["secretStore"].(map[string]any)
	if !ok {
		t.Fatal("chart has no secret store configuration")
	}
	audience, ok := store["audience"].(string)
	if !ok || audience == "" {
		t.Fatal("chart has no login audience")
	}
	return audience
}

func assertNoAuthoredStoreLogin(t *testing.T, signing *wfv1.Template) {
	t.Helper()
	for _, volume := range signing.Volumes {
		if volume.Projected == nil {
			continue
		}
		for _, source := range volume.Projected.Sources {
			if source.ServiceAccountToken != nil {
				t.Fatal("store login must be supplied by native delivery")
			}
		}
	}
}

func assertSigningCLI(t *testing.T, container *corev1.Container) {
	t.Helper()
	command := exec.CommandContext(t.Context(), signingCLI(t), append(container.Command[1:], container.Args...)...)
	command.Env = []string{"HOME=" + t.TempDir(), "PATH=" + os.Getenv("PATH"), "OBERTH_SECRETSTORE_CONFIG="}
	output, err := command.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 || !strings.Contains(string(output), "OBERTH_SECRETSTORE_CONFIG") {
		t.Fatalf("signing arguments do not reach the built CLI's delivery contract: %v %s", err, output)
	}
}

func signingCLI(t *testing.T) string {
	t.Helper()
	if path := os.Getenv("OBERTH_RELEASE_CLI"); path != "" {
		return path
	}
	path := filepath.Join(t.TempDir(), "oberth")
	command := exec.CommandContext(t.Context(), "go", "build", "-p=2", "-buildvcs=false", "-o", path, "../oberth")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatal(err, string(output))
	}
	return path
}

func TestSigningCLIRequiresSource(t *testing.T) {
	if os.Getenv("OBERTH_TEST_MISSING_SIGNING_CLI") == "1" {
		signingCLI(t)
		return
	}
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing-source")
	if err := os.Mkdir(missing, 0700); err != nil {
		t.Fatal(err)
	}
	writeFixtureCommand(t, filepath.Join(dir, "go"), `[ "$1" = build ]
echo 'missing CLI source build attempted' >&2
exit 1
`)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), binary, "-test.run=^TestSigningCLIRequiresSource$", "-test.v", "-test.timeout=30s")
	command.Dir = missing
	command.Env = append(os.Environ(), "OBERTH_TEST_MISSING_SIGNING_CLI=1", "OBERTH_RELEASE_CLI=", "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "missing CLI source build attempted") || strings.Contains(string(output), "SKIP") {
		t.Fatal("missing CLI source did not fail through the build", err, string(output))
	}
}
