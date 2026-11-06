package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/runlog"
)

func TestLogDefaultWindowAndExplicitFilters(t *testing.T) {
	for _, flags := range [][]string{nil, {"--context", "2"}, {"--pattern="}, {"--limit=0"}, {"--tail=false"}, {"--offset=0"}, {"--byte-offset=0"}, {"--tail", "--limit", "300"}} {
		t.Run(strings.Join(flags, " "), func(t *testing.T) {
			defaultWindow := len(flags) == 0 || flags[0] == "--context"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				q := r.URL.Query()
				if defaultWindow {
					if q.Get("tail") != "true" || q.Get("limit") != "200" {
						t.Error("missing default tail", q)
					}
				} else if q.Get("limit") == "200" {
					t.Error("explicit filter got default tail", q)
				}
				if len(flags) > 0 && flags[0] == "--byte-offset=0" && q.Get("byte_offset") != "0" {
					t.Error("lost zero cursor")
				}
				_, _ = w.Write([]byte(`{"output":"line\n","extra":9007199254740993}`))
			}))
			defer server.Close()
			configure(t, server)
			var out bytes.Buffer
			args := append(append([]string{}, flags...), "--json", "run-abc")
			if err := runRemoteLog(t.Context(), args, &out); err != nil {
				t.Fatal(err)
			}
			var got map[string]json.RawMessage
			if err := json.Unmarshal(out.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if string(got["extra"]) != "9007199254740993" {
				t.Fatal("lost unknown field or integer precision")
			}
			if defaultWindow {
				var window string
				if err := json.Unmarshal(got["window"], &window); err != nil || window != runlog.DefaultWindowDescription {
					t.Fatal("missing default explanation", out.String())
				}
			} else if _, ok := got["window"]; ok {
				t.Fatal("explicit filter labeled default")
			}
		})
	}
}
