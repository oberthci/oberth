package api

import (
	"github.com/oberthci/oberth/internal/publicerr"
	"strings"
	"testing"
)

func TestTransportMessageCatalogue(t *testing.T) {
	for key, m := range messages() {
		if err := publicerr.CheckMessage(m.reason, 80); err != nil {
			t.Errorf("%s reason: %v", key, err)
		}
		if err := publicerr.CheckMessage(m.remedy, 120); err != nil {
			t.Errorf("%s remedy: %v", key, err)
		}
	}
}

func TestSessionLimitRemediesNameAgentActions(t *testing.T) {
	for _, key := range []string{"sessionsIdentity", "sessionsInstallation", "eventsIdentity", "eventsInstallation"} {
		problem := refusal(key)
		if !strings.Contains(problem.Remedy(), "End an unused agent session") || strings.Contains(problem.Remedy(), "/mcp") {
			t.Fatal(key, problem.Remedy())
		}
		if strings.HasPrefix(key, "sessions") && !strings.Contains(problem.Remedy(), "wait an hour") {
			t.Fatal("session remedy omits the automatic expiry alternative", key, problem.Remedy())
		}
	}
}
