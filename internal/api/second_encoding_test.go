package api

import (
	"context"
	"encoding/json"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/publicerr"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type countedOutput struct {
	Calls   *int   `json:"-"`
	Payload string `json:"payload"`
}

func (v countedOutput) MarshalJSON() ([]byte, error) {
	*v.Calls++
	return json.Marshal(struct {
		Payload string `json:"payload"`
	}{v.Payload})
}
func TestSecondReplyEncodesPayloadOnce(t *testing.T) {
	for _, limit := range []int{15, 16} {
		calls := 0
		h := apicontract.HandlerFunc[apicontract.Empty, countedOutput](func(context.Context, apicontract.Caller, apicontract.Empty) (apicontract.Call[countedOutput], error) {
			return func(context.Context) (countedOutput, error) { return countedOutput{&calls, "xx"}, nil }, nil
		})
		reg, err := NewRegistry([]Tool{bindDefinition(apicontract.Binding[apicontract.Empty, countedOutput]{Handler: h}.Definition(apicontract.Metadata{Name: "encoding", Description: "Read a counted public payload.", Access: apicontract.Read, InputBytes: apicontract.RequestBytes, OutputBytes: limit, Deadline: time.Second}))})
		if err != nil {
			t.Fatal(err)
		}
		rt := Runtime{Now: func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }, Nonce: func() string { return "test" }, Ticker: func(d time.Duration) (<-chan time.Time, func()) { tick := time.NewTicker(d); return tick.C, tick.Stop }}
		s, err := New(Config{FailureLog: slog.New(slog.NewTextHandler(io.Discard, nil)), Auth: secondAuth{}, Registry: reg, Classify: publicerr.Classify, Runtime: rt})
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"encoding"}}`))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer test")
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if calls != 1 {
			t.Fatal("payload encoded more than once", calls)
		}
		if strings.Contains(w.Body.String(), `"isError":true`) != (limit == 15) {
			t.Fatal("wire byte limit differed from measured encoding", w.Body.String())
		}
	}
}
