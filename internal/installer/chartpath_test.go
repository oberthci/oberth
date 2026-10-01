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

// Without --chart nothing changes: the published chart at the requested version.
func TestChartValuesArgsStillReadThePublishedChart(t *testing.T) {
	t.Parallel()

	args := strings.Join(oberthChartValuesArgs(Config{ChartVersion: "v0.13.24"}), " ")
	if !strings.Contains(args, oberthRepoName+"/oberth") || !strings.Contains(args, "--version v0.13.24") {
		t.Errorf("published chart lookup changed: %s", args)
	}
}

// --image names the server image outright, so resolving a chart to discover one
// is asking a question that has already been answered.
func TestTLSBootstrapUsesTheGivenImageWithoutConsultingAnyChart(t *testing.T) {
	t.Parallel()
	pinned := "oberth-registry:5000/oberth@sha256:" + strings.Repeat("d", 64)

	deps := Deps{
		KindClusterName: "oberth",
		RunHelm: func(context.Context, []string) ([]byte, error) {
			return nil, errors.New("helm must not be consulted when --image names the image")
		},
		RunCommand: func(_ context.Context, _ []byte, name string, args ...string) ([]byte, error) {
			if name == "kind" && len(args) > 0 && args[0] == "get" {
				return []byte("oberth-control-plane\n"), nil
			}
			return nil, nil
		},
	}

	image, err := resolveOberthTLSBootstrapImage(context.Background(), Config{ImageRef: pinned}, deps)
	if err != nil {
		t.Fatalf("resolution failed: %v", err)
	}
	if image != pinned {
		t.Errorf("resolved %q, want the image that was given, %q", image, pinned)
	}
}
