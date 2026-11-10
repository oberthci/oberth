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

func TestRecordFieldsAreSnakeCaseUnlessGoNamesRequested(t *testing.T) {
	server, backend := testServer(t)
	backend.runPage = []model.Run{{ID: "aaaaaaaa", RepoID: 7, QueueSequence: 9007199254740993, Status: model.RunPassed, SHA: "commit"}}
	for _, test := range []struct {
		name    string
		path    string
		header  bool
		goNames bool
	}{
		{"array", "/api/runs?limit=1", false, false},
		{"envelope", "/api/runs?limit=1&envelope=1", false, false},
		{"go names header", "/api/runs?limit=1", true, true},
		{"go names query", "/api/runs?limit=1&go_field_names=1", false, true},
		{"go names envelope", "/api/runs?limit=1&envelope=1&go_field_names=1", false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, test.path, nil)
			req.Header.Set("Authorization", "Bearer valid-token")
			if test.header {
				req.Header.Set(GoFieldNamesHeader, "1")
			}
			out := httptest.NewRecorder()
			server.Handler().ServeHTTP(out, req)
			if out.Code != 200 {
				t.Fatal(out.Code, out.Body)
			}
			if got := out.Header().Get(GoFieldNamesHeader) == "1"; got != test.goNames {
				t.Fatalf("go names echo = %v", got)
			}
			var rows []map[string]json.RawMessage
			if strings.Contains(test.path, "envelope=1") {
				var object struct {
					Runs       []map[string]json.RawMessage `json:"runs"`
					NextBefore int64                        `json:"next_before"`
				}
				if err := json.Unmarshal(out.Body.Bytes(), &object); err != nil {
					t.Fatal(err)
				}
				rows = object.Runs
				if object.NextBefore != 9007199254740993 {
					t.Fatal("lost cursor", out.Body)
				}
			} else if err := json.Unmarshal(out.Body.Bytes(), &rows); err != nil {
				t.Fatalf("array changed: %v %s", err, out.Body)
			}
			row := rows[0]
			for old, alias := range map[string]string{"ID": "id", "RepoID": "repo_id", "QueueSequence": "queue_sequence", "Status": "status", "SHA": "sha"} {
				if len(row[alias]) == 0 {
					t.Errorf("missing %s: %s", alias, out.Body)
				}
				if _, present := row[old]; present != test.goNames || (test.goNames && string(row[old]) != string(row[alias])) {
					t.Errorf("go name %s present=%v: %s", old, present, out.Body)
				}
			}
			if string(row["run_id"]) != string(row["id"]) {
				t.Error("run_id differs from id", out.Body)
			}
		})
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

func TestCompatibilityLegacyCollectionsAndOptInEnvelopes(t *testing.T) {
	for _, tc := range []struct {
		path, field string
		value       any
	}{
		{"/api/repos", "repositories", []model.Repository{{ID: 1, Name: "oberth"}}},
		{"/api/grants", "grants", []map[string]any{}},
	} {
		for _, envelope := range []bool{false, true} {
			server, backend := testServer(t)
			switch tc.field {
			case "rows":
				backend.toolResult = tc.value
			case "repositories":
				server.views = &collectionViews{fakeBackend: backend, repos: tc.value}
			case "grants":
				server.views = &collectionViews{fakeBackend: backend, grants: tc.value}
			}
			path := tc.path
			if envelope {
				path += "?envelope=1"
			}
			request := httptest.NewRequest(http.MethodGet, path, nil)
			request.Header.Set("Authorization", "Bearer valid-token")
			out := httptest.NewRecorder()
			server.Handler().ServeHTTP(out, request)
			if out.Code != 200 {
				t.Fatalf("%s: %d %s", path, out.Code, out.Body)
			}
			var decoded any
			if err := json.Unmarshal(out.Body.Bytes(), &decoded); err != nil {
				t.Fatal(err)
			}
			if envelope {
				object, ok := decoded.(map[string]any)
				if !ok || object[tc.field] == nil {
					t.Fatalf("missing %s envelope: %s", tc.field, out.Body)
				}
			} else {
				if _, ok := decoded.([]any); !ok {
					t.Fatalf("legacy collection changed: %s", out.Body)
				}
			}
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
	raw, err := marshalCompatible(payload, true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "private-value") || strings.Contains(string(raw), "hidden") || !strings.Contains(string(raw), "9007199254740993") {
		t.Fatalf("alias privacy or precision: %s", raw)
	}
	promotion := PromotionListItem{ID: "promotion", RunID: "run"}
	raw, err = marshalCompatible(promotion, false)
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

func TestRunAliasSchemasRetainOldNamesAndAcceptPreferredName(t *testing.T) {
	for _, definition := range toolDefinitions() {
		name := definition["name"].(string)
		key := RunIDArgument(name)
		if key == "" {
			continue
		}
		schema := definition["inputSchema"].(map[string]any)
		properties := schema["properties"].(map[string]any)
		if properties[key] == nil || properties["run_id"] == nil {
			t.Fatalf("%s removed old selector or omitted alias", name)
		}
	}
}

func TestAdditiveInputSchemasAcceptOldAndPreferredArguments(t *testing.T) {
	for _, name := range []string{"run_get", "issue_update"} {
		var schema spec.Schema
		for _, definition := range toolDefinitions() {
			if definition["name"] == name {
				raw, _ := json.Marshal(definition["inputSchema"])
				if err := json.Unmarshal(raw, &schema); err != nil {
					t.Fatal(err)
				}
			}
		}
		var good, bad []string
		if name == "run_get" {
			good = []string{`{"id":"aaaaaaaa"}`, `{"run_id":"aaaaaaaa"}`, `{"id":"aaaaaaaa","run_id":"aaaaaaaa"}`}
			bad = []string{`{"run_id":4}`}
		} else {
			good = []string{`{"id":1}`, `{"id":1,"expected_revision":1}`}
			bad = []string{`{"id":1,"expected_revision":0}`, `{"id":1,"expected_revision":null}`, `{"id":1,"expected_revision":"1"}`}
		}
		for _, cases := range []struct {
			raw   []string
			valid bool
		}{{good, true}, {bad, false}} {
			for _, raw := range cases.raw {
				var args map[string]any
				if err := json.Unmarshal([]byte(raw), &args); err != nil {
					t.Fatal(err)
				}
				err := validate.AgainstSchema(&schema, args, strfmt.Default)
				if (err == nil) != cases.valid {
					t.Errorf("%s %s: %v (valid=%t)", name, raw, err, cases.valid)
				}
			}
		}
	}
}

func TestResourceQueryAcceptsRunIDAliasAndRejectsConflict(t *testing.T) {
	for _, tc := range []struct {
		query  string
		status int
	}{
		{"run_id=aaaaaaaa", 200}, {"run=aaaaaaaa&run_id=aaaaaaaa", 200}, {"run=bbbbbbbb&run_id=aaaaaaaa", 400},
	} {
		server, backend := testServer(t)
		request := httptest.NewRequest(http.MethodGet, "/api/run-resources?"+tc.query, nil)
		request.Header.Set("Authorization", "Bearer valid-token")
		out := httptest.NewRecorder()
		server.Handler().ServeHTTP(out, request)
		if out.Code != tc.status {
			t.Fatalf("%s: %d %s", tc.query, out.Code, out.Body)
		}
		if tc.status == 200 {
			var args map[string]any
			if err := json.Unmarshal(backend.calledArguments, &args); err != nil {
				t.Fatal(err)
			}
			if args["run"] != "aaaaaaaa" {
				t.Fatalf("lost alias: %s", backend.calledArguments)
			}
		} else if backend.calledName != "" {
			t.Fatal("conflicting alias reached backend")
		}
	}
}

// A full run_get used to carry every record twice (Steps and steps, Run and
// run). The default MCP reply now carries it once; the go_field_names opt-in still
// returns both spellings.
func TestRunGetMCPPayloadSendsRecordsOnce(t *testing.T) {
	steps := make([]model.StepResult, 41)
	for i := range steps {
		usage := make([]model.StepResourceUsage, 40)
		for j := range usage {
			usage[j] = model.StepResourceUsage{TemplateName: fmt.Sprintf("step-%d", i), ContainerKind: "main", PodKind: "workflow"}
		}
		steps[i] = model.StepResult{Stage: "ci", Step: fmt.Sprintf("step-%d", i), Ordinal: i, Status: model.StepPassed, MeasuredUsage: usage}
	}
	payload := RunDetailResponse{Run: model.Run{ID: "aaaaaaaa", RepoID: 1, Status: model.RunPassed, SHA: strings.Repeat("a", 40)}, Steps: steps, Timing: &model.RunTiming{}}
	size := func(goNames bool) (int, map[string]json.RawMessage) {
		server, backend := testServer(t)
		backend.toolResult = payload
		request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"run_get","arguments":{}}}`))
		request.Header.Set("Authorization", "Bearer valid-token")
		if goNames {
			request.Header.Set(GoFieldNamesHeader, "1")
		}
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
		return len(envelope.Result.Structured), object
	}
	current, object := size(false)
	goNamesSize, goNamesObject := size(true)
	t.Logf("run_get structuredContent: %d bytes snake_case only, %d bytes with Go-style names", current, goNamesSize)
	if object["Steps"] != nil || object["Run"] != nil || len(object["steps"]) == 0 || len(object["run"]) == 0 {
		t.Fatalf("default reply is not snake_case only: %v", keys(object))
	}
	if goNamesObject["Steps"] == nil || goNamesObject["steps"] == nil {
		t.Fatalf("go_field_names opt-in lost a spelling: %v", keys(goNamesObject))
	}
	if current*10 > goNamesSize*6 {
		t.Fatalf("snake_case reply %d bytes is not well under the Go-style %d bytes", current, goNamesSize)
	}
}

func keys(object map[string]json.RawMessage) []string {
	names := make([]string, 0, len(object))
	for name := range object {
		names = append(names, name)
	}
	return names
}

// A json tag that spells a lowerCamel wire name (queue_cap_config's
// maxNotStarted, a declaration's activeDeadlineSeconds) is the field's
// declared name, not a Go-style duplicate: it stays next to its snake_case
// spelling, so system_status keeps the queueCap shape it documents.
func TestLowerCamelTaggedNamesStayByDefault(t *testing.T) {
	payload := struct {
		Canonical *struct {
			MaxNotStarted int `json:"max_not_started"`
		} `json:"queue_cap,omitempty"`
		QueueCap model.QueueCapStatus `json:"queueCap"`
		RepoID   int64
	}{QueueCap: model.QueueCapStatus{MaxNotStarted: 3, NotStarted: 1}, RepoID: 7}
	raw, err := marshalCompatible(payload, false)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		QueueCap map[string]any `json:"queueCap"`
		Fields   map[string]any `json:"-"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &wire.Fields); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"maxNotStarted", "max_not_started", "notStarted", "not_started"} {
		if _, ok := wire.QueueCap[name]; !ok {
			t.Fatalf("queueCap lost %q: %s", name, raw)
		}
	}
	if _, goName := wire.Fields["RepoID"]; goName || wire.Fields["repo_id"] != float64(7) {
		t.Fatalf("Go-style name was not replaced by snake_case: %s", raw)
	}
}
