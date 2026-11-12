package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
)

func TestDashboardRunDetailOmitsHeavyDiagnosticsWithoutMutatingSource(t *testing.T) {
	server, backend := testServer(t)
	detail := RunDetailResponse{Run: model.Run{ID: "run-1"}, Timing: &model.RunTiming{}, Steps: []model.StepResult{{DeclaredSize: "M", Stage: "test", Step: "unit", MeasuredUsage: []model.StepResourceUsage{{}}, MeasuredSizeComparison: []model.ResourceSizeComparison{{}}, Termination: &model.StepDiagnostic{}}}}
	backend.runResult = detail
	request := httptest.NewRequest(http.MethodGet, "/api/runs/run-1?view=dashboard", nil)
	request.Header.Set("Authorization", "Bearer valid-token")
	writer := httptest.NewRecorder()
	server.Handler().ServeHTTP(writer, request)
	if writer.Code != http.StatusOK {
		t.Fatal(writer.Code, writer.Body.String())
	}
	for _, field := range []string{"measured_usage", "measured_size_comparison", "termination", "timing", "MeasuredUsage", "Timing", "DeclaredSize", "declared_size"} {
		if strings.Contains(writer.Body.String(), field) {
			t.Fatalf("dashboard retained %s", field)
		}
	}
	if !strings.Contains(writer.Body.String(), `"step":"unit"`) || strings.Contains(writer.Body.String(), `"Step"`) {
		t.Fatal(writer.Body.String())
	}
	// The default HTTP representation is also the CLI --json contract.
	request = httptest.NewRequest(http.MethodGet, "/api/runs/run-1", nil)
	request.Header.Set("Authorization", "Bearer valid-token")
	writer = httptest.NewRecorder()
	server.Handler().ServeHTTP(writer, request)
	if writer.Code != http.StatusOK {
		t.Fatal(writer.Code, writer.Body.String())
	}
	for _, field := range []string{"measured_usage", "measured_size_comparison", "termination", "timing"} {
		if !strings.Contains(writer.Body.String(), `"`+field+`"`) {
			t.Fatalf("default HTTP response lost %s", field)
		}
	}
	if strings.Contains(writer.Body.String(), "declared_size") {
		t.Fatal("default HTTP response retained declared size", writer.Body.String())
	}
	mcp := callOutputContract(t, "run_get", detail)
	if mcp.IsError || strings.Contains(string(mcp.Structured), "declared_size") || strings.Contains(string(mcp.Content), "declared_size") {
		t.Fatal("MCP retained obsolete size", mcp)
	}
	full, err := json.Marshal(detail)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"MeasuredUsage", "MeasuredSizeComparison", "Termination", "Timing"} {
		if !strings.Contains(string(full), field) {
			t.Fatalf("full/MCP value lost %s", field)
		}
	}
}
