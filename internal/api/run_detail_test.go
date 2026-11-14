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
	detail := RunDetailResponse{Run: model.Run{ID: "run-1"}, Timing: &model.RunTiming{}, Steps: []model.StepResult{{NodeID: "test.unit", Stage: "test", Step: "unit", Termination: &model.StepDiagnostic{}}}}
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
	for _, field := range []string{"termination", "timing"} {
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
	for _, field := range []string{"max_rss_bytes", "memory_peak_bytes", "user_cpu", "system_cpu", "measured_usage", "measured_size_comparison", "MaxRSSBytes", "MemoryPeakBytes", "UserCPU", "SystemCPU", "MeasuredUsage", "MeasuredSizeComparison"} {
		if strings.Contains(writer.Body.String(), field) || strings.Contains(string(mcp.Structured), field) || strings.Contains(string(mcp.Content), field) {
			t.Fatal("retired usage field exposed", field)
		}
	}
	full, err := json.Marshal(detail)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"Termination", "Timing"} {
		if !strings.Contains(string(full), field) {
			t.Fatalf("full/MCP value lost %s", field)
		}
	}
}

func TestRunReadersDoNotSerializeGrantEvidence(t *testing.T) {
	step := model.StepResult{NodeID: "node", Status: model.StepRunning, Credentialed: true, CredentialEvidenceKnown: true, ReplayEvidenceComplete: true, GrantEvidence: []string{"secret:private-path", "capability:private-account", "wif:private-role"}}
	detail := RunDetailResponse{Run: model.Run{ID: "run"}, Steps: []model.StepResult{step}}
	raw, err := json.Marshal(detail)
	if err != nil {
		t.Fatal(err)
	}
	dashboard, err := json.Marshal(dashboardRunDetail(detail))
	if err != nil {
		t.Fatal(err)
	}
	mcp := callOutputContract(t, "run_get", detail)
	if mcp.IsError {
		t.Fatal(mcp)
	}
	for _, body := range []string{string(raw), string(dashboard), string(mcp.Structured), string(mcp.Content)} {
		for _, private := range []string{"private-path", "private-account", "private-role", "grant_evidence", "GrantEvidence", "credential_evidence_known", "CredentialEvidenceKnown", "replay_evidence_complete", "ReplayEvidenceComplete"} {
			if strings.Contains(body, private) {
				t.Fatal("private grant evidence reached ordinary run reader", private)
			}
		}
	}
	if len(step.GrantEvidence) != 3 || !step.CredentialEvidenceKnown {
		t.Fatal("serialization changed replay evidence")
	}
}

func TestRunReaderExitAvailability(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		detail := RunDetailResponse{Steps: []model.StepResult{{NodeID: "node", Status: model.StepFailed, ExitCodeUnknown: unknown, Termination: &model.StepDiagnostic{}}}}
		full, err := marshalRecords(detail)
		if err != nil {
			t.Fatal(err)
		}
		dashboard, err := marshalRecords(dashboardRunDetail(detail))
		if err != nil {
			t.Fatal(err)
		}
		mcp := callOutputContract(t, "run_get", detail)
		if mcp.IsError {
			t.Fatal(mcp)
		}
		var content []struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(mcp.Content, &content); err != nil || len(content) == 0 {
			t.Fatal("missing MCP text", err)
		}
		for _, raw := range []string{string(full), string(dashboard), string(mcp.Structured), content[0].Text} {
			if strings.Contains(raw, `"exit_code":0`) == unknown {
				t.Fatal("exit availability lost on wire", raw)
			}
		}
		if strings.Contains(string(dashboard), `"termination"`) {
			t.Fatal("dashboard exposed heavy diagnostic", string(dashboard))
		}
	}
}
