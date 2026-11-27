package installer

import (
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestHostReviewRequiresCompleteThresholdsAndPreservesReserves(t *testing.T) {
	for _, field := range []string{"maxPods", "systemReserved", "enforceNodeAllocatable", "evictionHard", "evictionMinimumReclaim", "imageMaximumGCAge", "containerLogMaxFiles"} {
		t.Run(field, func(t *testing.T) {
			config := hostFixture(t)
			delete(config, field)
			if got := strings.Join(ReviewHost(config), "\n"); !strings.Contains(got, "Host field: "+field) {
				t.Fatal("missing host setting passed review", got)
			}
		})
	}
	config := hostFixture(t)
	delete(config["evictionHard"].(map[string]any), "imagefs.inodesFree")
	if !strings.Contains(strings.Join(ReviewHost(config), "\n"), "Host field: evictionHard") {
		t.Fatal("omitted inode eviction threshold passed review")
	}
	config = hostFixture(t)
	config["kubeReserved"] = map[string]any{"cpu": "500m"}
	if !strings.Contains(strings.Join(ReviewHost(config), "\n"), "Host field: kubeReserved") {
		t.Fatal("additional reserve passed review")
	}
}

func TestHostProfileCompleteNativeMap(t *testing.T) {
	config := hostFixture(t)
	if got := ReviewHost(config); len(got) != 1 || !strings.Contains(got[0], "matches") {
		t.Fatal(got)
	}
	want := map[string]any{"memory.available": "1Gi", "nodefs.available": "10%", "imagefs.available": "10%", "nodefs.inodesFree": "5%", "imagefs.inodesFree": "5%"}
	if !reflectHostEqual(config["evictionHard"], want) || config["maxPods"] != float64(110) {
		t.Fatal("profile lost native threshold map or Pod ceiling", config)
	}
}

func hostFixture(t *testing.T) map[string]any {
	t.Helper()
	var config map[string]any
	if err := yaml.Unmarshal([]byte(HostProfile), &config); err != nil {
		t.Fatal(err)
	}
	return config
}

func reflectHostEqual(a, b any) bool {
	left, _ := yaml.Marshal(a)
	right, _ := yaml.Marshal(b)
	return string(left) == string(right)
}
