package measured

import "testing"

func TestReviewCurrentOmittedLimitsAreUnlimited(t *testing.T) {
	for _, resource := range []string{"memory", "ephemeral-storage"} {
		t.Run(resource, func(t *testing.T) {
			d := Declaration{Stage: "step-12345678", Template: "step-abc12345", Resources: map[string]map[string]string{"limits": {"memory": "2Gi", "ephemeral-storage": "512Mi"}}}
			control := Build(observations(), []Declaration{d})[0]
			if control.Suggested == nil {
				t.Fatalf("positive control refused: %v", control.NoSuggestion)
			}
			delete(d.Resources["limits"], resource)
			got := Build(observations(), []Declaration{d})[0]
			if got.Suggested != nil {
				t.Fatalf("current %s limit absent but finite advice returned: %+v", resource, got.Suggested)
			}
		})
	}
}
