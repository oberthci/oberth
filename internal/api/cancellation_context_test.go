package api

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/publicerr"
)

func TestOnlyClientOwnedCancellationIsQuiet(t *testing.T) {
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	expired, stop := context.WithDeadline(t.Context(), time.Unix(0, 0))
	defer stop()
	serverStopped := context.WithValue(cancelled, budgetKey{}, &deliveryBudget{request: httptest.NewRequest("GET", "/", nil)})
	for _, scenario := range []struct {
		name, level string
		request     context.Context
		cause       error
		code        publicerr.Code
		drain       bool
	}{
		{"client", "", cancelled, context.Canceled, publicerr.Unavailable, false},
		{"server", "ERROR", t.Context(), context.Canceled, publicerr.Invariant, false},
		{"server operation context", "ERROR", serverStopped, context.Canceled, publicerr.Invariant, false},
		{"request deadline", "ERROR", expired, context.Canceled, publicerr.Invariant, false},
		{"unrelated client failure", "ERROR", cancelled, errors.New("server operation failed"), publicerr.Invariant, false},
		{"read budget", "WARN", t.Context(), deadlineError(apicontract.Metadata{}), publicerr.Unavailable, false},
		{"mutation budget", "WARN", t.Context(), deadlineError(apicontract.Metadata{Mutation: true, Check: "run_get"}), publicerr.Unavailable, false},
		{"draining", "WARN", t.Context(), context.Canceled, publicerr.Unavailable, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			var logs bytes.Buffer
			s := &Server{config: Config{Classify: publicerr.Classify, FailureLog: slog.New(slog.NewJSONHandler(&logs, nil)), Runtime: Runtime{Now: func() time.Time { return time.Unix(1, 0) }, Nonce: func() string { return "failure-reference" }}}}
			s.draining.Store(scenario.drain)
			problem := s.publicError(scenario.request, scenario.cause)
			if problem.Code() != scenario.code {
				t.Fatal("cancellation changed public classification", problem)
			}
			if scenario.level == "" {
				if logs.Len() != 0 || problem.Reason() != "Request interrupted." {
					t.Fatal("client cancellation was not quiet", logs.String(), problem)
				}
			} else if !strings.Contains(logs.String(), `"level":"`+scenario.level+`"`) || !strings.Contains(logs.String(), `"reason":"`+problem.Reason()+`"`) {
				t.Fatal("server cancellation lost its logged reason", logs.String(), problem)
			}
		})
	}
}
