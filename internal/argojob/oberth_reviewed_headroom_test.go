package argojob

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

// Retained peaks from branch a5144ee1989c, release 398379142b7c and last-30.
// These are lower bounds; regression floors do not claim complete cold-run coverage.
func TestOberthReviewedPeakHeadroom(t *testing.T) {
	for _, tc := range []struct {
		file, name           string
		memory, storage, cpu int64
	}{
		{"build.yaml", "build-amd64", 0, 0, 1000},
		{"build.yaml", "decompress-shellcheck", 0, 0, 1000},
		{"build.yaml", "test-gitcache", 819073024, 227926016, 0},
		{"build.yaml", "test-openbao", 3288342528, 3586068480, 0},
		{"build.yaml", "test-service", 1999200256, 453992448, 0},
		{"build.yaml", "verify-shellcheck", 0, 0, 353},
		{"build.yaml", "vet", 0, 0, 1000},
		{"release.yaml", "copy-test-tools", 4832141312, 425656320, 880},
		{"release.yaml", "install-cosign", 0, 0, 1000},
		{"release.yaml", "release-chart-test", 0, 0, 856},
		{"release.yaml", "release-test-gitcache", 941920256, 1865973760, 0},
		{"release.yaml", "release-test-rest", 6509568000, 6073286656, 0},
		{"release.yaml", "release-test-service", 4294975488, 3629645824, 0},
		{"release.yaml", "release-test-store", 3680964608, 2774695936, 0},
	} {
		t.Run(tc.file+"/"+tc.name, func(t *testing.T) {
			workflow, err := argoworkflow.Decode(loadPipeline(t, tc.file))
			if err != nil {
				t.Fatal(err)
			}
			for _, leaf := range workflow.Spec.Templates {
				if leaf.Name != tc.name {
					continue
				}
				var resources corev1.ResourceRequirements
				if leaf.Container != nil {
					resources = leaf.Container.Resources
				} else if leaf.Script != nil {
					resources = leaf.Script.Resources
				} else {
					t.Fatal("missing main container")
				}
				if resources.Requests.Memory().Value() < tc.memory || resources.Limits.Memory().Value()*10 < tc.memory*13 {
					t.Fatalf("memory below observed peak/headroom: %+v", resources)
				}
				if resources.Limits.StorageEphemeral().Value()*2 < tc.storage*3 {
					t.Fatalf("storage below observed headroom: %+v", resources)
				}
				if resources.Requests.Cpu().MilliValue() < tc.cpu {
					t.Fatalf("CPU below capped observed average: %+v", resources)
				}
				return
			}
			t.Fatal("missing reviewed template")
		})
	}
}
