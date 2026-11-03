package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"testing"
)

func TestDashboardIssueWorkspaceBehavior(t *testing.T) {
	output, err := exec.Command("node", "../../hack/test-issue-workspace.mjs").CombinedOutput()
	if err != nil {
		t.Fatalf("issue workspace dashboard: %v\n%s", err, output)
	}
}

func TestIssueWorkspaceHTTPFiltersAndDetailOverride(t *testing.T) {
	for _, raw := range []string{"work_status=active&stale_after=2h&active_stale_after=5m", "work_status=todo", "work_status=waiting", "work_status=review"} {
		request := httptest.NewRequest(http.MethodGet, "/api/issues?"+raw, nil)
		filter, err := issueFilter(request)
		if err != nil || filter.WorkStatus == "" {
			t.Fatalf("%s: %+v %v", raw, filter, err)
		}
		if filter.WorkStatus == "active" && (filter.StaleAfter != "2h" || filter.ActiveStaleAfter != "5m") {
			t.Fatalf("overrides lost: %+v", filter)
		}
	}
	for _, raw := range []string{"work_status=invalid", "stale_after=0s", "active_stale_after=-1m", "stale_after=garbage"} {
		if _, err := issueFilter(httptest.NewRequest(http.MethodGet, "/api/issues?"+raw, nil)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	server, backend := testServer(t)
	backend.toolResult = map[string]any{"id": 42, "work_status": "active", "stale": true}
	request := httptest.NewRequest(http.MethodGet, "/api/issues/42?stale_after=2h&active_stale_after=5m", nil)
	request.Header.Set("Authorization", "Bearer valid-token")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	var args map[string]any
	if err := json.Unmarshal(backend.calledArguments, &args); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || args["stale_after"] != "2h" || args["active_stale_after"] != "5m" {
		t.Fatalf("detail override=%v %s", args, response.Body)
	}
}
