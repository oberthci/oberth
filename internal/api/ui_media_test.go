package api_test

import (
	"bytes"
	"net/http/httptest"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
)

func TestMCPPostRequiresJSON(t *testing.T) {
	for _, media := range []string{"", "text/plain", "application/x-www-form-urlencoded", "multipart/form-data; boundary=x", "application/json", "application/json; charset=utf-8", "application/json; charset", "application/json; charset="} {
		t.Run(media, func(t *testing.T) {
			f := setup(t, nil)
			h := f.bindings.RunCancel.Handler.(*fake.Handler[apicontract.RunCancelArgs, apicontract.Receipt])
			r := httptest.NewRequest("POST", "/mcp", bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"run_cancel","arguments":{"id":"12345678","reason":"web UI"}}}`))
			r.Header.Set("Authorization", "Bearer valid")
			r.Header.Set("Content-Type", media)
			w := httptest.NewRecorder()
			f.server.Handler().ServeHTTP(w, r)
			_, _, calls := h.Observed()
			valid := media == "application/json" || media == "application/json; charset=utf-8"
			if valid {
				assertSuccess(t, w)
				if calls != 1 {
					t.Fatal("JSON did not reach the handler once")
				}
			} else if w.Code != 400 || calls != 0 {
				t.Fatalf("non-JSON reached handler: status %d calls %d", w.Code, calls)
			}
		})
	}
	f := setup(t, nil)
	h := f.bindings.RunCancel.Handler.(*fake.Handler[apicontract.RunCancelArgs, apicontract.Receipt])
	w := request(f, "POST", "/mcp?extra=true", []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"run_cancel","arguments":{"id":"12345678"}}}`))
	_, _, calls := h.Observed()
	if w.Code != 400 || calls != 0 {
		t.Fatal("query arguments reached MCP handler", w.Code, calls)
	}

}
