package argoworkflow

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The upstream utility has a larger module graph than Oberth's production
// readers. Keep its test-only graph in a disposable module, using the pinned
// dependency versions, rather than expanding the shipped dependency manifests.
func TestRefusalUppercaseLeafPodName(t *testing.T) {
	directory := t.TempDir()
	for _, name := range []string{"go.mod", "go.sum"} {
		body, err := os.ReadFile(filepath.Join("..", "..", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, name), body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(directory, "pod_name_test.go"), []byte(uppercaseLeafPodNameProof), 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), "go", "test", "-mod=mod", "-p=2", "-run", "^TestUppercaseLeafPodName$", "-count=1", "-v")
	command.Dir = directory
	command.Env = append(os.Environ(), "GOMAXPROCS=2", "GOWORK=off", "GOENV=off")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("upstream Pod-name proof failed: %v\n%s", err, output)
	}
	t.Logf("%s", output)
}

const uppercaseLeafPodNameProof = `package proof

import (
 "strings"
 "testing"
 argoutil "github.com/argoproj/argo-workflows/v4/workflow/util"
 k8svalidation "k8s.io/apimachinery/pkg/util/validation"
)

func TestUppercaseLeafPodName(t *testing.T) {
 t.Setenv("POD_NAMES", "v2")
 version := argoutil.GetPodNameVersion()
 if version != argoutil.PodNameV2 { t.Fatalf("Pod naming version = %s", version) }
 upper := argoutil.GeneratePodName("workflow", "workflow.run", "camelCase", "node-id", version)
 if !strings.Contains(upper, "-camelCase-") { t.Fatalf("Argo changed the authored template name: %q", upper) }
 if problems := k8svalidation.IsDNS1123Subdomain(upper); len(problems) == 0 { t.Fatalf("Kubernetes unexpectedly admits uppercase Pod name %q", upper) }
 lower := argoutil.GeneratePodName("workflow", "workflow.run", "lowercase", "node-id", version)
 if problems := k8svalidation.IsDNS1123Subdomain(lower); len(problems) != 0 { t.Fatalf("lowercase control %q: %v", lower, problems) }
 root := argoutil.GeneratePodName("workflow", "workflow", "camelCase", "node-id", version)
 if root != "workflow" { t.Fatalf("entrypoint Pod name = %q", root) }
 t.Logf("Argo v4.0.8 v2 descendant Pod %q is rejected by Kubernetes; root Pod %q is valid", upper, root)
}
`
