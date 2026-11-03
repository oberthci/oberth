package api

import (
	"bytes"
	"log"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAuthenticationFailureLogEscapesControlCharacters(t *testing.T) {
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(previous) })
	request := httptest.NewRequest("GET", "/", nil)
	request.RemoteAddr = "192.0.2.1:1234"
	var server Server
	server.logAuthFailure(request, "invalid\r\nforged=success\t\"quoted\"")
	line := output.String()
	if strings.Count(line, "\n") != 1 || strings.ContainsAny(strings.TrimSuffix(line, "\n"), "\r\n\t") {
		t.Fatalf("authentication failure escaped the log record: %q", line)
	}
	if !strings.Contains(line, `reason="invalid\r\nforged=success\t\"quoted\""`) || !strings.Contains(line, `peer="192.0.2.1"`) {
		t.Fatalf("authentication fields are not quoted: %q", line)
	}
}
