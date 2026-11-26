package api_test

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestMCPRunWaitReturnsFailedAndCancelledRunsWithoutRefusal(t *testing.T) {
	for _, status := range []stepmodel.RunStatus{stepmodel.RunFailed, stepmodel.RunCancelled} {
		t.Run(string(status), func(t *testing.T) {
			f := setup(t, func(_ *api.Config, b *apicontract.Bindings) {
				b.RunWait.Handler = &fake.Handler[apicontract.RunWaitArgs, apicontract.WaitResult]{Value: apicontract.WaitResult{Run: &stepmodel.RunSummary{ID: "deadbeef", Status: status}}}
			})
			w := rpc(f, "run_wait", map[string]any{"id": "deadbeef"})
			assertSuccess(t, w)
			if !strings.Contains(w.Body.String(), `"status":"`+string(status)+`"`) {
				t.Fatal("run outcome missing from MCP result", w.Body.String())
			}
		})
	}
}
