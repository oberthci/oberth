package runner

import (
	"os"
	"strings"
	"testing"
)

func TestRunnerImageVerificationIsFailClosed(t *testing.T) {
	t.Parallel()
	gatePath := "../../hack/verify-runner-image.sh"
	gateRaw, err := os.ReadFile(gatePath)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(gatePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatal("runner image gate is not executable")
	}
	gate := string(gateRaw)
	for _, required := range []string{
		`flock -n 9`,
		`write_state failed`,
		`write_state passed`,
		`refusing to authorize an image from a dirty source tree`,
		`--platform linux/amd64`,
		`--provenance=false`,
		`--no-cache`,
		`rewrite-timestamp=true`,
		`runner.repro.oci`,
		`repeated build produced a different manifest digest`,
		`test "$image_user" = 65534:65534`,
		`--scanners vuln,secret`,
		`--severity HIGH,CRITICAL`,
		`--exit-code 1`,
		`usr/local/bin/oberth-runner`,
		`forbidden baked tool found`,
		`forbidden baked package found`,
		`python3`,
		`python-pkg`,
		`compressedLayerBytes`,
		`104857600`,
		`database_age`,
		`259200`,
		`5b3ebab0f98d95196c85efc3a9d31a01520c96fa342e4e611f56db64c516df1d`,
		`.ArtifactType == "container_image" and .Metadata.ImageID == $image_id`,
		`published_manifest`,
		`.passed == true and .scanner.exitCode == 0`,
	} {
		if !strings.Contains(gate, required) {
			t.Fatalf("runner image gate lacks %q", required)
		}
	}
	if strings.Count(gate, "docker buildx build \\\n") != 2 {
		t.Fatalf("runner image gate must build exactly twice for reproducibility")
	}
	for _, forbidden := range []string{
		"--target tools-export", "--target trivydb", "BUILD_CACHE_NAMESPACE",
		"--offline-scan", "--skip-db-update", "--skip-java-db-update", "docker run",
	} {
		if strings.Contains(gate, forbidden) {
			t.Fatalf("runner image gate retains fat/offline behavior %q", forbidden)
		}
	}

	makefileRaw, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	makefile := string(makefileRaw)
	if !strings.Contains(makefile, "images: verify-runner-image") ||
		!strings.Contains(makefile, "verify-runner-image: provision-runner-trivy") {
		t.Fatal("default image workflow does not provision the external scanner and require the runner gate")
	}

	provisionPath := "../../hack/provision-runner-trivy.sh"
	provisionRaw, err := os.ReadFile(provisionPath)
	if err != nil {
		t.Fatal(err)
	}
	provisionInfo, err := os.Stat(provisionPath)
	if err != nil {
		t.Fatal(err)
	}
	if provisionInfo.Mode().Perm()&0o111 == 0 {
		t.Fatal("runner Trivy provisioner is not executable")
	}
	provision := string(provisionRaw)
	for _, required := range []string{
		`releases/download/v0.73.0/trivy_0.73.0_Linux-64bit.tar.gz`,
		`2edd39da482bb4e9831962487b68f68e3928ec3137794757f54d00383d79547b`,
		`5b3ebab0f98d95196c85efc3a9d31a01520c96fa342e4e611f56db64c516df1d`,
		`Version: 0.73.0`,
	} {
		if !strings.Contains(provision, required) {
			t.Fatalf("runner Trivy provisioner lacks %q", required)
		}
	}

	evidenceRaw, err := os.ReadFile("../../hack/verify-runner-evidence.sh")
	if err != nil {
		t.Fatal(err)
	}
	evidence := string(evidenceRaw)
	for _, required := range []string{
		`flock -s -n 9`,
		`deployment requires a clean source tree`,
		`.status == "passed"`,
		`runner-oci.py`,
		`OCI graph validation failed`,
		`chart runner digest does not match`,
		`usr/local/bin/oberth-runner`,
		`forbidden baked tool`,
		`forbidden baked package`,
		`python3`,
		`python-pkg`,
		`compressedLayerBytes`,
		`104857600`,
	} {
		if !strings.Contains(evidence, required) {
			t.Fatalf("runner evidence gate lacks %q", required)
		}
	}
}
