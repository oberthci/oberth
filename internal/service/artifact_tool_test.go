package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
)

func TestArtifactToolsAcceptEveryParameterTheirSchemaAdvertises(t *testing.T) {
	t.Parallel()
	service := &API{}
	for name, arguments := range map[string]string{
		"artifacts":         `{"id":"run-abc"}`,
		"artifact_get":      `{"id":"run-abc","name":"report.xml","pattern":"FAIL","context":2,"offset":0,"limit":10,"tail":true}`,
		"artifact_get_bare": `{"id":"run-abc","name":"report.xml"}`,
	} {
		tool := strings.TrimSuffix(name, "_bare")
		_, err := service.CallTool(context.Background(), api.Actor{Identity: "tester"}, tool, json.RawMessage(arguments))
		if err == nil {
			continue
		}
		if strings.Contains(err.Error(), "unknown field") {
			t.Fatalf("%s rejects a parameter its schema advertises: %v", tool, err)
		}
		if !strings.Contains(err.Error(), "artifacts") && !strings.Contains(err.Error(), "unavailable") {
			t.Fatalf("%s failed for an unexpected reason: %v", tool, err)
		}
	}
}

func TestArtifactGetRefusesAnUnknownParameter(t *testing.T) {
	t.Parallel()
	service := &API{}
	_, err := service.CallTool(context.Background(), api.Actor{Identity: "tester"},
		"artifact_get", json.RawMessage(`{"id":"run-abc","name":"r","nonsense":true}`))
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("artifact_get accepted an undeclared parameter: %v", err)
	}
}
