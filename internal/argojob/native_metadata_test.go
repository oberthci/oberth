package argojob

import (
	"encoding/json"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
)

func TestNativeAuthorLabelsAndAntiAffinityReachWorkflowAPI(t *testing.T) {
	wf := nativeWorkflow(t)
	wf.Spec.PodMetadata = &wfv1.Metadata{Labels: map[string]string{"app": "store"}, Annotations: map[string]string{"example.test/owner": "team"}}
	wf.Spec.Templates[0].Metadata = wfv1.Metadata{Labels: map[string]string{"security": "S2"}, Annotations: map[string]string{"example.test/note": "authored"}}
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
	if submitted.Spec.PodMetadata.Annotations["example.test/owner"] != "team" || submitted.Spec.Templates[0].Metadata.Annotations["example.test/note"] != "authored" {
		t.Fatal("author annotations lost")
	}
	// Feed this exact posted Workflow to the pinned controller Pod regression.
	encoded, err := json.Marshal(submitted)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("POD_METADATA_PROOF=%s", encoded)
}
