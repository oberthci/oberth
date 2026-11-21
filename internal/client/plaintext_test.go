package client

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
)

func TestLoopbackHTTPNeverTransmitsBearer(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"structuredContent":{"repositories":[]}}}`)
	}))
	defer server.Close()
	c, err := New(Config{Options: Options{URL: server.URL, TokenFile: privateFile(t, "bearer", []byte("private-bearer"))}, Now: fake.NewClock().Now})
	if err == nil {
		defer c.Close()
		_, _, _ = c.Call(t.Context(), definition(t, "repo_list"), json.RawMessage(`{}`))
	}
	if err == nil || calls.Load() != 0 {
		t.Fatal("plaintext address reached the bearer transport", calls.Load(), err)
	}
}
