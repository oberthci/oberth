package api_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/apicontract"
)

func TestRequestedWaitControlsPreparationDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		parent, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		outer, _ := parent.Deadline()
		var deadline time.Time
		f := setup(t, func(_ *api.Config, b *apicontract.Bindings) {
			b.RunWait.Handler = apicontract.HandlerFunc[apicontract.RunWaitArgs, apicontract.WaitResult](func(ctx context.Context, _ apicontract.Caller, _ apicontract.RunWaitArgs) (apicontract.Call[apicontract.WaitResult], error) {
				deadline, _ = ctx.Deadline()
				return func(context.Context) (apicontract.WaitResult, error) {
					return apicontract.WaitResult{TimedOut: true}, nil
				}, nil
			})
		})
		r := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"run_wait","arguments":{"id":"12345678","timeout_s":1}}}`)).WithContext(parent)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer valid")
		w := httptest.NewRecorder()
		f.server.Handler().ServeHTTP(w, modernRequest(r))
		assertSuccess(t, w)
		if outer.Sub(deadline) != 4*time.Second {
			t.Fatal("request timeout ignored", outer.Sub(deadline))
		}
	})
}
