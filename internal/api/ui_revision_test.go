package api_test

import (
	"regexp"
	"testing"
)

func TestUIRevisionBindsTheShellAndAPIResponses(t *testing.T) {
	f := setup(t, nil)
	shell := uiRequest(f, "/runs")
	match := regexp.MustCompile(`<meta name="oberth-ui" content="([a-f0-9]{64})">`).FindStringSubmatch(shell.Body.String())
	if len(match) != 2 || shell.Header().Get("X-Oberth-UI") != match[1] {
		t.Fatal("shell lacks its UI revision")
	}
	for _, path := range []string{"/api/status", "/api/runs", "/assets/app.js", "/status"} {
		if response := uiRequest(f, path); response.Header().Get("X-Oberth-UI") != match[1] {
			t.Fatal("response does not identify the current UI", path)
		}
	}
}
