package argojob

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"

	"github.com/oberthci/oberth/pkg/pipeline"
)

func TestNativeAuthorLabelsAndAntiAffinityReachWorkflowAPI(t *testing.T) {
	wf := nativeWorkflow(t)
	wf.Spec.Templates[0].Container.Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")}
	wf.Spec.PodMetadata = &wfv1.Metadata{Labels: map[string]string{"app": "store"}}
	wf.Spec.Templates[0].Metadata = wfv1.Metadata{Labels: map[string]string{"security": "S2"}}
	wf.Spec.Affinity = nativeExample(t, "../../pkg/argoworkflow/testdata/native/placement-06.yaml").Affinity
	submitted := nativeSubmit(t, wf)
	nativeEqual(t, wf.Spec.Affinity, submitted.Spec.Affinity)
	if submitted.Spec.PodMetadata.Labels["app"] != "store" || submitted.Spec.Templates[0].Metadata.Labels["security"] != "S2" {
		t.Fatal("author labels lost")
	}
	for _, key := range []string{runLabel, triggerLabel, tierLabel} {
		if submitted.Spec.PodMetadata.Labels[key] == "" {
			t.Fatal("server label lost", key)
		}
	}
	// Feed this exact posted Workflow to the pinned controller Pod regression.
	encoded, err := json.Marshal(submitted)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("POD_METADATA_PROOF=%s", encoded)
}

func TestBuildRefusesAuthorPodAnnotationBehaviorSwitches(t *testing.T) {
	for _, site := range []string{"workflow", "template", "artifactGC"} {
		for _, key := range []string{"container.apparmor.security.beta.kubernetes.io/main", "k8s.v1.cni.cncf.io/networks", "cni.projectcalico.org/ipAddrs", "sidecar.istio.io/inject", "vault.hashicorp.com/agent-inject", "io.kubernetes.cri-o.Devices", "kubernetes.io/egress-bandwidth"} {
			t.Run(fmt.Sprintf("%v/%s", site, key), func(t *testing.T) {
				wf := nativeWorkflow(t)
				metadata := wfv1.Metadata{Annotations: map[string]string{key: "unconfined"}}
				switch site {
				case "template":
					wf.Spec.Templates[0].Metadata = metadata
				case "artifactGC":
					wf.Spec.ArtifactGC = &wfv1.WorkflowLevelArtifactGC{ArtifactGC: wfv1.ArtifactGC{PodMetadata: &metadata}}
				default:
					wf.Spec.PodMetadata = &metadata
				}
				raw, err := yaml.Marshal(wf)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = Build(seedTestConfig(), testRequest(pipeline.TriggerCI, string(raw))); err == nil || !strings.Contains(err.Error(), "annotations is assigned by Oberth") {
					t.Fatal("Build admitted author Pod annotation", err)
				}
			})
		}
	}
}

func TestArtifactGCPodLabelsReachWorkflowAPI(t *testing.T) {
	wf := nativeWorkflow(t)
	wf.Spec.ArchiveLogs = ptr.To(true)
	wf.Spec.ArtifactGC = &wfv1.WorkflowLevelArtifactGC{ArtifactGC: wfv1.ArtifactGC{Strategy: wfv1.ArtifactGCOnWorkflowCompletion, PodMetadata: &wfv1.Metadata{Labels: map[string]string{"app": "gc-worker"}}}}
	posted := nativeSubmit(t, wf)
	if posted.Spec.ArtifactGC.PodMetadata.Labels["app"] != "gc-worker" || len(posted.Spec.ArtifactGC.PodMetadata.Annotations) != 0 {
		t.Fatal("artifactGC metadata changed", posted.Spec.ArtifactGC)
	}
}
