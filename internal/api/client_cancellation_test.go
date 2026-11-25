package api_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/publicerr"
)

func TestCancelledRequestDoesNotLogAnInternalError(t *testing.T) {
	for _, transport := range []string{"json", "sse", "log"} {
		t.Run(transport, func(t *testing.T) {
			logs, body := cancellationExchange(t, transport, true, fmt.Errorf("read ended: %w", context.Canceled))
			if logs != "" || strings.Contains(body, "Reference:") {
				t.Fatal("client cancellation became an internal failure", logs, body)
			}
		})
	}
}

func TestServerCancellationsKeepClassificationAndLogs(t *testing.T) {
	for _, scenario := range []struct {
		name, level, reason string
		cause               error
	}{
		{"cancel", "ERROR", "Operation failed.", context.Canceled},
		{"deadline", "ERROR", "Operation failed.", context.DeadlineExceeded},
		{"labelled cancel", "WARN", "Backend stopped.", publicerr.New(publicerr.Unavailable, "Backend stopped.", "Retry the observation.", context.Canceled)},
		{"labelled deadline", "WARN", "Backend stopped.", publicerr.New(publicerr.Unavailable, "Backend stopped.", "Retry the observation.", context.DeadlineExceeded)},
		{"joined unknown cancel", "ERROR", "Operation failed.", errors.Join(publicerr.New(publicerr.Unavailable, "Backend stopped.", "Retry the observation.", nil), context.Canceled)},
	} {
		for _, transport := range []string{"json", "sse", "log"} {
			t.Run(scenario.name+"/"+transport, func(t *testing.T) {
				logs, body := cancellationExchange(t, transport, false, scenario.cause)
				if !strings.Contains(logs, `"level":"`+scenario.level+`"`) || !strings.Contains(logs, `"reason":"`+scenario.reason+`"`) || !strings.Contains(body, scenario.reason) || strings.Contains(body, "Request interrupted.") {
					t.Fatal("server cancellation lost its classification or log reason", logs, body)
				}
			})
		}
	}
}

func cancellationExchange(t *testing.T, transport string, client bool, cause error) (string, string) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	fail := func() error {
		if client {
			cancel()
		}
		return cause
	}
	var logs bytes.Buffer
	f := setup(t, func(c *api.Config, b *apicontract.Bindings) {
		c.FailureLog = slog.New(slog.NewJSONHandler(&logs, nil))
		b.RunWait.Handler = &fake.Handler[apicontract.RunWaitArgs, apicontract.WaitResult]{Run: func(context.Context) (apicontract.WaitResult, error) { return apicontract.WaitResult{}, fail() }}
		c.Streams = &fake.Streams{Open: func(context.Context, apicontract.Caller, apicontract.ContainerArgs) (io.ReadCloser, error) {
			return nil, fail()
		}}
	})
	r := httptest.NewRequest("POST", "/mcp", strings.NewReader(waitBody())).WithContext(ctx)
	r.Header.Set("Content-Type", "application/json")
	switch transport {
	case "sse":
		r.Header.Set("Accept", "text/event-stream")
	case "log":
		r = httptest.NewRequest("GET", "/api/runs/12345678/containerlog/stream?node_id=node&container=main", nil).WithContext(ctx)
	}
	r.Header.Set("Authorization", "Bearer valid")
	w := newStreamWriter()
	f.server.Handler().ServeHTTP(w, r)
	return logs.String(), w.Body.String()
}
