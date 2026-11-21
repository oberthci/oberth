package nativebinding

import (
	"math"
	"testing"
)

func TestNativeMetadataGoldens(t *testing.T) {
	for _, tc := range []struct{ got, want string }{
		{RunLabel, "oberth.ci/run"}, {EngineLabel, "oberth.ci/engine"}, {EngineValue, "argo"},
		{RepositoryLabel, "oberth.ci/repo"}, {CommitLabel, "oberth.ci/sha"}, {TriggerLabel, "oberth.ci/trigger"},
		{TierLabel, "oberth.ci/tier"}, {RunIDAnnotation, "oberth.ci/run-id"}, {RefAnnotation, "oberth.ci/ref"},
		{IdentityAnnotation, "oberth.ci/spec-identity"},
		{NativeCronLabel, "workflows.argoproj.io/cron-workflow"}, {ManagedLabel, "app.kubernetes.io/managed-by"}, {ManagedValue, "oberth"},
		{TierValue(false), "ci"}, {TierValue(true), "release"},
	} {
		if tc.got != tc.want {
			t.Errorf("metadata = %q, want %q", tc.got, tc.want)
		}
	}
}

func TestCronNameFixedGoldens(t *testing.T) {
	for _, tc := range []struct {
		id          int64
		entry, want string
	}{
		{math.MaxInt64, "nightly", "oberth-cron-1y2p0ij32e8e7-2a3b62b53ddb9f167b63d222"},
		{1, "nightly", "oberth-cron-1-2a3b62b53ddb9f167b63d222"},
	} {
		got, err := CronName(tc.id, tc.entry)
		if err != nil || got != tc.want {
			t.Fatalf("CronName = %q, %v; want %q", got, err, tc.want)
		}
	}
}
