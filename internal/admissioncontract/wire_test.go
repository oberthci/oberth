package admissioncontract

import (
	"reflect"
	"strings"
	"testing"
)

// TestWireTags pins admission wire fields, including each finding's exact fix.
func TestWireTags(t *testing.T) {
	cases := []struct {
		value  any
		fields string
	}{
		{PreparedView{}, "Workflow:workflow SpecDigest:spec_digest Digest:digest Repository:repo RunID:run_id Ref:ref ObjectSHA:object_sha CommitSHA:commit_sha Trigger:trigger ScheduleEntry:schedule_entry SelectedFile:selected_file FileBlob:file_blob SourceReceipt:source_receipt SourceBindings:source_bindings Defaults:defaults Templates:templates Identity:identity Grants:grants Plan:plan Findings:findings"},
		{TransformInput{}, "Plan: Mode: Identity: CredentialSteps: SecretGrants: SourceBindings: Defaults:"},
		{Plan{}, "Entries:entries Exact:exact"},
		{PlanEntry{}, "Path:plan_path Name:name Template:- FanOut:fan_out,omitempty Dynamic:dynamic ParallelGroup:parallel_group,omitempty CredentialIntent:credential_intent SecretRoute:secret_route,omitempty OutputParameters:output_parameters,omitempty ResultConsumed:result_consumed,omitempty HoldArmed:hold_armed,omitempty HoldEligible:hold_eligible,omitempty HoldRefusal:hold_refusal,omitempty"},
		{PrepareRequest{}, "Mode:mode Repository:repo RunID:run_id Ref:ref ObjectSHA:object_sha CommitSHA:commit_sha Trigger:trigger ScheduleEntry:schedule_entry SelectedFile:selected_file FileBlob:file_blob Document:document Defaults:defaults Templates:templates SourceReceipt:source_receipt IdentityRequest:identity_request Grants:grants"},
		{IdentityRequest{}, "Mode:mode Namespace:namespace PipelineAccount:pipeline_account ResourceAccount:resource_account"},
		{TemplateSnapshot{}, "Namespace:namespace Name:name UID:uid ResourceVersion:resource_version Spec:spec"},
		{AccountEvidence{}, "Name:name UID:uid"},
		{IdentityEvidence{}, "Namespace:namespace Pipeline:pipeline Resource:resource,omitempty State:state"},
		{GrantEvidence{}, "Repository:repo File:file Step:step Kind:kind Path:path Audience:audience Revision:revision State:state"},
		{Finding{}, "Advisory:advisory,omitempty Installation:installation,omitempty Path:path Code:code Reason:reason Remedy:remedy Command:command,omitempty"},
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
