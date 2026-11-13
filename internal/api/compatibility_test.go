package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-openapi/spec"
	"github.com/go-openapi/strfmt"
	"github.com/go-openapi/validate"

	"github.com/oberthci/oberth/internal/model"
)

func TestRecordFieldsAreSnakeCase(t *testing.T) {
	server, backend := testServer(t)
	backend.runPage = []model.Run{{ID: "aaaaaaaa", RepoID: 7, QueueSequence: 9007199254740993, Status: model.RunPassed, SHA: "commit"}}
	for _, path := range []string{"/api/runs?limit=1", "/api/runs?limit=1&envelope=0"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer valid-token")
		out := httptest.NewRecorder()
		server.Handler().ServeHTTP(out, req)
		if out.Code != 200 {
			t.Fatal(out.Code, out.Body)
		}
		var object struct {
			Runs       []map[string]json.RawMessage `json:"runs"`
			NextBefore int64                        `json:"next_before"`
		}
		if err := json.Unmarshal(out.Body.Bytes(), &object); err != nil || len(object.Runs) != 1 {
			t.Fatalf("not an envelope: %v %s", err, out.Body)
		}
		if object.NextBefore != 9007199254740993 {
			t.Fatal("lost cursor", out.Body)
		}
		row := object.Runs[0]
		for old, name := range map[string]string{"ID": "id", "RepoID": "repo_id", "QueueSequence": "queue_sequence", "Status": "status", "SHA": "sha"} {
			if len(row[name]) == 0 {
				t.Errorf("missing %s: %s", name, out.Body)
			}
			if _, present := row[old]; present {
				t.Errorf("go name %s present: %s", old, out.Body)
			}
		}
		if _, alias := row["run_id"]; alias {
			t.Error("run record still carries the run_id identity alias", out.Body)
		}
	}
}

func TestCompatibilityAddsObjectAliasesWithoutChangingDictionaryKeys(t *testing.T) {
	payload := struct {
		Run    model.Run
		Labels map[string]string
	}{Run: model.Run{ID: "aaaaaaaa"}, Labels: map[string]string{"BuildMain": "kept"}}
	result := callOutputContract(t, "run_get", payload)
	var object map[string]json.RawMessage
	if err := json.Unmarshal(result.Structured, &object); err != nil {
		t.Fatal(err)
	}
	if len(object["run"]) == 0 || object["Run"] != nil {
		t.Fatal("MCP run is not snake_case only", string(result.Structured))
	}
	var labels map[string]string
	if err := json.Unmarshal(object["labels"], &labels); err != nil {
		t.Fatal(err)
	}
	if len(labels) != 1 || labels["BuildMain"] != "kept" {
		t.Fatal("changed dictionary keys", labels)
	}
	if !strings.Contains(string(result.Content), `\"run\"`) {
		t.Fatal("text and structured content disagree", string(result.Content))
	}
}

func TestCollectionsAreAlwaysEnvelopes(t *testing.T) {
	for _, tc := range []struct {
		path, field string
		value       any
	}{
		{"/api/repos", "repositories", []model.Repository{{ID: 1, Name: "oberth"}}},
		{"/api/grants", "grants", []map[string]any{}},
	} {
		server, backend := testServer(t)
		if tc.field == "repositories" {
			server.views = &collectionViews{fakeBackend: backend, repos: tc.value}
		} else {
			server.views = &collectionViews{fakeBackend: backend, grants: tc.value}
		}
		request := httptest.NewRequest(http.MethodGet, tc.path, nil)
		request.Header.Set("Authorization", "Bearer valid-token")
		out := httptest.NewRecorder()
		server.Handler().ServeHTTP(out, request)
		if out.Code != 200 {
			t.Fatalf("%s: %d %s", tc.path, out.Code, out.Body)
		}
		var object map[string]any
		if err := json.Unmarshal(out.Body.Bytes(), &object); err != nil || object[tc.field] == nil {
			t.Fatalf("missing %s envelope: %v %s", tc.field, err, out.Body)
		}
	}
}

type collectionViews struct {
	*fakeBackend
	repos, grants any
}

func (v *collectionViews) Repositories(context.Context, Actor) (any, error) { return v.repos, nil }
func (v *collectionViews) Grants(context.Context, Actor, GrantFilter) (any, error) {
	return v.grants, nil
}

func TestCompatibilityKeepsHiddenFieldsAndCanonicalIDsDistinct(t *testing.T) {
	// Hidden server-owned values must never be recovered by alias generation.
	payload := struct {
		ID     string
		Hidden string `json:"-"`
		Labels map[string]any
	}{ID: "public", Hidden: "private-value", Labels: map[string]any{"UserKey": int64(9007199254740993)}}
	raw, err := marshalRecords(payload)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "private-value") || strings.Contains(string(raw), "hidden") || !strings.Contains(string(raw), "9007199254740993") {
		t.Fatalf("alias privacy or precision: %s", raw)
	}
	promotion := PromotionListItem{ID: "promotion", RunID: "run"}
	raw, err = marshalRecords(promotion)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err = json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["id"] != "promotion" || decoded["run_id"] != "run" {
		t.Fatal("related run overwrote promotion identity", string(raw))
	}
}

// Every run selector is id; no tool publishes a run_id or run alias of it.
func TestRunSelectorsAreID(t *testing.T) {
	for _, definition := range toolDefinitions() {
		name := definition["name"].(string)
		properties := definition["inputSchema"].(map[string]any)["properties"].(map[string]any)
		if _, alias := properties["run_id"]; alias {
			t.Errorf("%s publishes run_id", name)
		}
	}
	for _, name := range []string{"run_get", "wait", "run_wait"} {
		for _, definition := range toolDefinitions() {
			if definition["name"] != name {
				continue
			}
			properties := definition["inputSchema"].(map[string]any)["properties"].(map[string]any)
			if properties["id"] == nil || properties["run"] != nil {
				t.Errorf("%s selector: %v", name, properties)
			}
		}
	}
	var schema spec.Schema
	for _, definition := range toolDefinitions() {
		if definition["name"] == "run_get" {
			raw, _ := json.Marshal(definition["inputSchema"])
			if err := json.Unmarshal(raw, &schema); err != nil {
				t.Fatal(err)
			}
		}
	}
	for raw, valid := range map[string]bool{`{"id":"aaaaaaaa"}`: true, `{"run_id":"aaaaaaaa"}`: false} {
		var args map[string]any
		if err := json.Unmarshal([]byte(raw), &args); err != nil {
			t.Fatal(err)
		}
		if err := validate.AgainstSchema(&schema, args, strfmt.Default); (err == nil) != valid {
			t.Errorf("run_get %s: %v", raw, err)
		}
	}
}

func TestRemovedResourceReportsHaveNoHTTPRoute(t *testing.T) {
	server, _ := testServer(t)
	for _, path := range []string{"/api/run-resources?run=aaaaaaaa", "/api/repos/repo/usage"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("Authorization", "Bearer valid-token")
		out := httptest.NewRecorder()
		server.Handler().ServeHTTP(out, request)
		if out.Code != http.StatusNotFound {
			t.Fatal("removed report route remains", path, out.Code)
		}
	}
}

// A full run_get used to carry every record twice (Steps and steps, Run and
// run). The MCP reply carries it once.
func TestRunGetMCPPayloadSendsRecordsOnce(t *testing.T) {
	steps := make([]model.StepResult, 41)
	for i := range steps {
		steps[i] = model.StepResult{NodeID: ("ci") + "." + (fmt.Sprintf("step-%d", i)), Stage: "ci", Step: fmt.Sprintf("step-%d", i), Ordinal: i, Status: model.StepPassed}
	}
	payload := RunDetailResponse{Run: model.Run{ID: "aaaaaaaa", RepoID: 1, Status: model.RunPassed, SHA: strings.Repeat("a", 40)}, Steps: steps, Timing: &model.RunTiming{}}
	size := func() map[string]json.RawMessage {
		server, backend := testServer(t)
		backend.toolResult = payload
		request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"run_get","arguments":{}}}`))
		request.Header.Set("Authorization", "Bearer valid-token")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		var envelope struct {
			Result outputContractResult `json:"result"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		var object map[string]json.RawMessage
		if err := json.Unmarshal(envelope.Result.Structured, &object); err != nil {
			t.Fatal(err)
		}
		return object
	}
	object := size()
	if object["Steps"] != nil || object["Run"] != nil || len(object["steps"]) == 0 || len(object["run"]) == 0 {
		t.Fatalf("default reply is not snake_case only: %v", keys(object))
	}
}

func keys(object map[string]json.RawMessage) []string {
	names := make([]string, 0, len(object))
	for name := range object {
		names = append(names, name)
	}
	return names
}

// A json tag spelled in lowerCamel (a declaration's activeDeadlineSeconds)
// is sent under its snake_case name only.
func TestLowerCamelTaggedNamesAreSnakeCase(t *testing.T) {
	payload := struct {
		Deadline int `json:"activeDeadlineSeconds"`
		RepoID   int64
	}{Deadline: 30, RepoID: 7}
	raw, err := marshalRecords(payload)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if _, camel := fields["activeDeadlineSeconds"]; camel || fields["active_deadline_seconds"] != float64(30) {
		t.Fatalf("lowerCamel name: %s", raw)
	}
	if _, goName := fields["RepoID"]; goName || fields["repo_id"] != float64(7) {
		t.Fatalf("Go-style name was not replaced by snake_case: %s", raw)
	}

}
