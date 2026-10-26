package installer

import (
	"testing"

	"github.com/oberthci/oberth/internal/containerimage"
)

func TestRestoreHelperSeparatesServerSchemaFromFileTools(t *testing.T) {
	server := serverDeployment{Image: "example.invalid/scratch@sha256:fixture", ClaimName: "data"}
	pod := restoreHelperPod("restore", "oberth", "stamp", server)
	if len(pod.Spec.InitContainers) != 1 || len(pod.Spec.Containers) != 1 {
		t.Fatal("missing schema or utility container")
	}
	for _, container := range pod.Spec.InitContainers {
		if container.Image == server.Image && (len(container.Command) == 0 || container.Command[0] != "/usr/local/bin/oberth") {
			t.Fatalf("non-native server helper: %+v", container)
		}
	}
	if pod.Spec.InitContainers[0].Image != server.Image || pod.Spec.InitContainers[0].Args[0] != "version" {
		t.Fatal("schema probe doesn't use target server image")
	}
	if pod.Spec.Containers[0].Image != containerimage.Utility {
		t.Fatal("restore shell uses server image")
	}
}
