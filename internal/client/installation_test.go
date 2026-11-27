package client

import (
	"net/http"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
)

func TestInstallationStateSurvivesMCPReleaseBoundary(t *testing.T) {
	for _, version := range []string{"v0.30.18", "v0.30.19"} {
		t.Run(version, func(t *testing.T) {
			b := fake.Bindings()
			b.AdminVersion.Handler = &fake.Handler[apicontract.AdminVersionArgs, apicontract.SystemStatus]{Value: apicontract.SystemStatus{Version: version, SchemaVersion: 6}}
			remote := installationRemote(t, b)
			status, err := remote.InstallationState(t.Context())
			if err != nil || status.Version != version || status.SchemaVersion != 6 {
				t.Fatal(status, err)
			}
		})
	}
}

func installationRemote(t *testing.T, b apicontract.Bindings) *Client {
	t.Helper()
	h := server(t, b)
	remote, err := New(Config{Options: Options{URL: "https://oberth.local/mcp", TokenFile: privateFile(t, "access", []byte("valid"))}, Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/mcp" {
			t.Fatal("installation depends on matching MCP versions")
		}
		return handlerTransport(h)(r)
	})})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(remote.Close)
	return remote
}
