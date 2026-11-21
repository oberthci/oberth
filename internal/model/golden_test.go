package model

import (
	"encoding/json"
	"testing"
)

func TestModelWireGoldens(t *testing.T) {
	title, body, upstream, revision, status := "title", "body", "origin", int64(2), IssueActive
	cases := []struct {
		name   string
		value  any
		golden string
	}{
		{"run origin", RunOrigin{}, `{"actor":"","trigger":"","schedule_entry":"","assist_requested":false,"assisted":false,"rerun_of":"","workflow_name":"","workflow_uid":""}`},
		{"publication", Publication{}, `{"id":"","repo":"","ref":"","run_id":"","promotion_id":"","object_sha":"","commit_sha":null,"previous_sha":null,"actor":"","status":"","attempts":0,"retry_at":null,"reason":"","created_at":"0001-01-01T00:00:00Z","updated_at":"0001-01-01T00:00:00Z"}`},
		{"audit action", AuditAction{}, `{"id":0,"actor":"","action":"","resource_type":"","resource_id":"","details":"","created_at":"0001-01-01T00:00:00Z"}`},
		{"CI request", CIRequest{}, `{"receive_id":"","repo":"","ref":"","previous_sha":"","object_sha":"","commit_sha":null,"actor":{"auth_method":"","identity":"","fingerprint":"","admin":false,"scope":null},"trigger":"","schedule_entry":"","assist":false}`},
		{"issue", Issue{}, `{"id":0,"revision":0,"kind":"","title":"","body":"","state":"","work_status":"","upstream":"","repo":"","branch":"","occurrences":0,"ci_origin":"","ci_work_id":"","stale":false,"lock":null,"created_at":"0001-01-01T00:00:00Z","updated_at":"0001-01-01T00:00:00Z","closed_at":null}`},
		{"issue lock", IssueLock{}, `{"owner":"","acquired_at":"0001-01-01T00:00:00Z","expires_at":"0001-01-01T00:00:00Z"}`},
		{"issue patch", IssuePatch{ID: 1, Title: &title, Body: &body, Upstream: &upstream, ExpectedRevision: &revision, ExpectedBodySHA256: "digest", WorkStatus: &status}, `{"id":1,"expected_revision":2,"expected_body_sha256":"digest","title":"title","body":"body","work_status":"active","upstream":"origin"}`},
		{"issue filter", IssueFilter{Repo: "origin/org/repo", WorkStatus: IssueActive, Kind: IssueCI, State: "all", Query: "needle", Before: 3, Limit: 20, StaleAfter: "168h", ActiveStaleAfter: "1h"}, `{"repo":"origin/org/repo","work_status":"active","kind":"ci","state":"all","query":"needle","before":3,"limit":20,"stale_after":"168h","active_stale_after":"1h"}`},
		{"invitation", Invitation{}, `{"id":"","identity":"","server_url":"","ca_pin":"","scope":null,"expires_at":"0001-01-01T00:00:00Z"}`},
		{"invitation file", InvitationFile{}, `{"id":"","identity":"","server_url":"","ca_pin":"","scope":null,"expires_at":"0001-01-01T00:00:00Z","known_hosts":"","secret":""}`},
		{"redemption", InviteRedemption{}, `{"secret":"","server_url":"","ca_pin":"","csr":"","ssh_public_key":""}`},
		{"invite certificate", InviteCertificate{}, `{"identity":"","scope":null,"ssh_fingerprint":"","ssh_url":"","certificate":""}`},
		{"event page", EventPage{Events: []AgentEvent{}, Gap: true}, `{"events":[],"next_after_id":0,"latest_id":0,"oldest_id":0,"has_more":false,"gap":true}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(tc.value)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != tc.golden {
				t.Fatalf("wire changed:\n got %s\nwant %s", data, tc.golden)
			}
		})
	}
}
func TestPublicationPredecessorStates(t *testing.T) {
	absent := ""
	object := "0123456789abcdef"
	for _, tc := range []struct {
		value *string
		json  string
	}{{nil, "null"}, {&absent, `""`}, {&object, `"0123456789abcdef"`}} {
		data, err := json.Marshal(Publication{PreviousSHA: tc.value}.Clone())
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err = json.Unmarshal(data, &fields); err != nil {
			t.Fatal(err)
		}
		if string(fields["previous_sha"]) != tc.json {
			t.Fatal("predecessor knowledge changed")
		}
	}
}
func TestFalseGapIsOmitted(t *testing.T) {
	data, err := json.Marshal(EventPage{})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["gap"]; ok {
		t.Fatal("false gap is not omitted")
	}
}
