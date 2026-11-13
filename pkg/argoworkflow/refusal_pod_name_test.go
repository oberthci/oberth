package argoworkflow

import (
	"strings"
	"testing"

	argoutil "github.com/argoproj/argo-workflows/v4/workflow/util"
	k8svalidation "k8s.io/apimachinery/pkg/util/validation"
)

func TestRefusalUppercaseLeafPodName(t *testing.T) {
	t.Setenv("POD_NAMES", "v2")
	version := argoutil.GetPodNameVersion()
	if version != argoutil.PodNameV2 {
		t.Fatalf("Pod naming version = %s", version)
	}
	upper := argoutil.GeneratePodName("workflow", "workflow.run", "camelCase", "node-id", version)
	if !strings.Contains(upper, "-camelCase-") {
		t.Fatalf("Argo changed the authored template name: %q", upper)
	}
	if problems := k8svalidation.IsDNS1123Subdomain(upper); len(problems) == 0 {
		t.Fatalf("Kubernetes unexpectedly admits uppercase Pod name %q", upper)
	}
	lower := argoutil.GeneratePodName("workflow", "workflow.run", "lowercase", "node-id", version)
	if problems := k8svalidation.IsDNS1123Subdomain(lower); len(problems) != 0 {
		t.Fatalf("lowercase control %q: %v", lower, problems)
	}
	root := argoutil.GeneratePodName("workflow", "workflow", "camelCase", "node-id", version)
	if root != "workflow" {
		t.Fatalf("entrypoint Pod name = %q", root)
	}
	t.Logf("Argo v4.0.8 v2 descendant Pod %q is rejected by Kubernetes; root Pod %q is valid", upper, root)
}
