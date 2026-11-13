package argojob

import (
	"testing"

	"github.com/oberthci/oberth/pkg/pipeline"
)

func TestOberthReleaseKeepsWorkflowAndPublicationLocks(t *testing.T) {
	workflow, err := Build(oberthConfig(), Request{
		RunID: "run-locks", Name: "oberth-release-locks", Repo: "oberth",
		UpstreamOrg: "acme", UpstreamName: "github", Ref: "refs/tags/v0.22.0",
		SHA: testSHA, Trigger: pipeline.TriggerRelease,
		Source: loadPipeline(t, "release.yaml"), ApprovedSecrets: oberthReleaseApprovedSecrets(),
	})
	if err != nil {
		t.Fatal(err)
	}
	locks := workflow.Spec.Synchronization
	if locks == nil || len(locks.Mutexes) != 1 || locks.Mutexes[0] == nil {
		t.Fatal("release lost whole-Workflow serialization")
	}
	// All releases share this authored Workflow lock after deployment.
	// Before this patch only the publication-template locks are present.
	name := locks.Mutexes[0].Name
	if name != "oberth-release" {
		t.Fatalf("release Workflow mutex is not a fixed shared name: %q", name)
	}
	for template, want := range map[string]string{
		"release-publish-chart":    "oberth-chart-index",
		"release-public-artifacts": "oberth-release-finalize",
	} {
		found := false
		for _, candidate := range workflow.Spec.Templates {
			if candidate.Name != template {
				continue
			}
			found = true
			lock := candidate.Synchronization
			if lock == nil || len(lock.Mutexes) != 1 || lock.Mutexes[0] == nil || lock.Mutexes[0].Name != want || want == name {
				t.Fatalf("%s lost its independent publication mutex", template)
			}
		}
		if !found {
			t.Fatalf("missing publication template %s", template)
		}
	}
}
