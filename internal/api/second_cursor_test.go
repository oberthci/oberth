package api_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
)

func TestWaitCeilingPreservesRequestedEventCursor(t *testing.T) {
	for _, sse := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			f := setup(t, func(c *api.Config, b *apicontract.Bindings) {
				c.MaximumWait = time.Millisecond
				b.EventsWait.Handler = &fake.Handler[apicontract.EventsWaitArgs, apicontract.EventResult]{Run: func(ctx context.Context) (apicontract.EventResult, error) {
					<-ctx.Done()
					return apicontract.EventResult{}, ctx.Err()
				}}
			})
			r := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"events_wait","arguments":{"after_id":12345}}}`))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Authorization", "Bearer valid")
			if sse {
				r.Header.Set("Accept", "text/event-stream")
			}
			w := newStreamWriter()
			f.server.Handler().ServeHTTP(w, modernRequest(r))
			if !strings.Contains(w.Body.String(), `"timed_out":true`) || !strings.Contains(w.Body.String(), `"next_after_id":12345`) {
				t.Fatal("timeout rewound the durable event cursor", w.Body.String())
			}
		})
	}
}
