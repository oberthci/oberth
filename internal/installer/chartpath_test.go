package installer

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// --chart says to install from a local directory instead of the published
// repository, but the OpenBao TLS bootstrap resolved the published chart anyway
// to find a server image. On a fork, or on any build whose version is not a
// published chart version, that lookup fails and takes the whole install with
// it, having already created the cluster.
func TestChartValuesArgsReadTheLocalChartWhenOneWasGiven(t *testing.T) {
	t.Parallel()

	args := strings.Join(oberthChartValuesArgs(Config{
		ChartPath:    "/somewhere/charts/oberth",
		ChartVersion: "v0.13.30-tz",
	}), " ")

	if !strings.Contains(args, "/somewhere/charts/oberth") {
		t.Errorf("values are read from the published repository despite --chart: %s", args)
	}
	if strings.Contains(args, "--version") {
		t.Errorf("a local chart directory has no repository version to select: %s", args)
	}
}

// Without --chart the values come from the verified chart of the signed
// release, never from a chart repository.
func TestChartValuesArgsReadTheVerifiedReleaseChart(t *testing.T) {
	t.Parallel()

	args := strings.Join(oberthChartValuesArgs(Config{ChartVersion: "v0.13.24", verifiedChart: "/private/oberth-0.13.24.tgz"}), " ")
	if args != "show values /private/oberth-0.13.24.tgz" {
		t.Errorf("chart values lookup = %s", args)
	}
	if args := strings.Join(oberthChartValuesArgs(Config{ChartVersion: "v0.13.24"}), " "); strings.Contains(args, oberthRepoName) {
		t.Errorf("unverified plan reads the chart repository: %s", args)
	}
}

// --image names the server image outright, so resolving a chart to discover one
// is asking a question that has already been answered.
func TestTLSBootstrapUsesTheGivenImageWithoutConsultingAnyChart(t *testing.T) {
	t.Parallel()
	pinned := "oberth-registry:5000/oberth@sha256:" + strings.Repeat("d", 64)

	deps := testAdminDeps(Deps{
		RunHelm: func(context.Context, []string) ([]byte, error) {
			return nil, errors.New("helm must not be consulted when --image names the image")
		},
		RunCommand: func(context.Context, []byte, string, ...string) ([]byte, error) {
			t.Fatal("explicit image resolution invoked a cluster manager")
			return nil, nil
		},
	})

	image, err := resolveOberthTLSBootstrapImage(context.Background(), Config{ImageRef: pinned}, deps)
	if err != nil {
		t.Fatalf("resolution failed: %v", err)
	}
	if image != pinned {
		t.Errorf("resolved %q, want the image that was given, %q", image, pinned)
	}
}

func TestHelmChartVersionDropsTagPrefix(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]string{
		"v0.17.26": "0.17.26",
		"0.17.26":  "0.17.26",
		"":         "",
		"latest":   "latest",
	} {
		if got := helmChartVersion(raw); got != want {
			t.Errorf("helmChartVersion(%q) = %q, want %q", raw, got, want)
		}
	}
}
