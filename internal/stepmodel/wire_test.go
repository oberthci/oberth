package stepmodel

import (
	"reflect"
	"strings"
	"testing"
)

// TestWireTags pins the fields and exact tags in docs/plans/README.md.
func TestWireTags(t *testing.T) {
	cases := []struct {
		value  any
		fields string
	}{
		{StepAttempt{}, "NodeID:node_id PodName:pod_name Status:status"},
		{LogicalStepCounts{}, "Total:step_count Exact:steps_planned_exact Pending:pending Queued:queued Running:running Passed:passed Failed:failed Skipped:skipped Unknown:unknown"},
		{RetryHint{}, "Action:action ReasonCode:reason_code After:after,omitempty"},
		{FailureRecord{}, "Class:failure_class Step:failed_step Name:failed_step_name NodeID:node_id Retry:retry_hint"},
		{LogSelector{}, "NodeID:node_id PodName:pod_name Container:container"},
		{RunStep{}, "Path:plan_path Name:name ParallelGroup:parallel_group,omitempty CredentialIntent:credential_intent Status:status DisplayReason:display_reason Detail:detail,omitempty StartedAt:started_at FinishedAt:finished_at AttemptCount:attempt_count Attempts:attempts,omitzero Logs:logs,omitzero"},
		{RunSummary{}, "ID:id Repo:repo Ref:ref ObjectSHA:object_sha CommitSHA:commit_sha Status:status DisplayReason:display_reason Detail:detail,omitempty NativePhase:native_phase,omitempty CreatedAt:created_at StartedAt:started_at FinishedAt:finished_at LogicalStepCounts: Steps:steps,omitzero NextStepOffset:next_step_offset,omitempty Failure:failure,omitempty"},
	}
	for _, tc := range cases {
		typ := reflect.TypeOf(tc.value)
		t.Run(typ.Name(), func(t *testing.T) {
			expected := strings.Fields(tc.fields)
			if typ.NumField() != len(expected) {
				t.Fatalf("field count = %d, want %d", typ.NumField(), len(expected))
			}
			for i, entry := range expected {
				name, tag, _ := strings.Cut(entry, ":")
				field := typ.Field(i)
				if field.Name != name || field.Tag.Get("json") != tag {
					t.Errorf("field %d = %s json:%q, want %s json:%q", i, field.Name, field.Tag.Get("json"), name, tag)
				}
			}
		})
	}
}
