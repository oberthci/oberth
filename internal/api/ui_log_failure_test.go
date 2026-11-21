package api_test

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/logcontract"
)

func TestBrowserAndCLIReceiveTheSameFailureContainer(t *testing.T) {
	f := setup(t, func(_ *api.Config, b *apicontract.Bindings) {
		b.RunLogs.Handler = &fake.Handler[apicontract.RunLogsArgs, apicontract.LogResult]{Value: apicontract.LogResult{ID: "12345678", Meta: logcontract.Meta{Copying: true}, Content: "artifact failure\n", Containers: []apicontract.Container{{NodeID: "failed", Container: "init-artifact-oberth"}}}}
	})
	for _, body := range []string{
		request(f, "GET", "/api/runs/12345678/logs?step=build", nil).Body.String(),
		rpc(f, "run_logs", map[string]string{"id": "12345678", "step": "build"}).Body.String(),
	} {
		if !strings.Contains(body, "artifact failure") || !strings.Contains(body, "init-artifact-oberth") || !strings.Contains(body, `"copying":true`) {
			t.Fatal("failure source was lost by a transport", body)
		}
	}
	_, source, _ := uiSources(t)
	if !strings.Contains(source, `"Container: " + item.container`) {
		t.Fatal("automatic failure selection is hidden in the browser")
	}
}
