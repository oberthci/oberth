package api_test

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func TestJSONBodyLimitsAcceptNRefuseNPlusOne(t *testing.T) {
	for _, tc := range []struct {
		name, path, base string
		limit            int
	}{
		{"ordinary", "/mcp", `{"jsonrpc":"2.0","id":1,"method":"ping"}`, 1 << 20},
		{"admin", "/api/admin/version", `{}`, 128 << 10},
		{"enrollment", "/mcp/enroll", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"uplink_redeem","arguments":{"secret":"invitation","csr":"csr","server_url":"https://oberth.ci/mcp","ca_pin":"pin","ssh_public_key":"public"}}}`, 96 << 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t, nil)
			body := append([]byte(tc.base), bytes.Repeat([]byte(" "), tc.limit-len(tc.base))...)
			assertSuccess(t, request(f, "POST", tc.path, body))
			if w := request(f, "POST", tc.path, append(body, ' ')); w.Code != 413 || !strings.Contains(w.Body.String(), fmt.Sprintf("Limit: %d bytes", tc.limit)) {
				t.Fatalf("N+1: %d %s", w.Code, w.Body.String())
			}
		})
	}
}
func TestPublicArgumentBounds(t *testing.T) {
	tests := []struct {
		name, tool string
		at, over   map[string]any
	}{
		{"run page", "run_list", map[string]any{"limit": 100}, map[string]any{"limit": 101}},
		{"step page", "run_get", map[string]any{"id": "12345678", "step_limit": 100}, map[string]any{"id": "12345678", "step_limit": 101}},
		{"wait seconds", "run_wait", map[string]any{"id": "12345678", "timeout_s": 600}, map[string]any{"id": "12345678", "timeout_s": 601}},
		{"exec seconds", "run_hold_exec", map[string]any{"id": "12345678", "step": "build", "argv": []string{"true"}, "timeout_s": 900}, map[string]any{"id": "12345678", "step": "build", "argv": []string{"true"}, "timeout_s": 901}},
		{"context lines", "run_logs", map[string]any{"id": "12345678", "context": 50}, map[string]any{"id": "12345678", "context": 51}},
		{"pattern bytes", "run_logs", map[string]any{"id": "12345678", "pattern": strings.Repeat("a", 512)}, map[string]any{"id": "12345678", "pattern": strings.Repeat("a", 513)}},
		{"event seconds", "events_wait", map[string]any{"timeout_s": 600}, map[string]any{"timeout_s": 601}},
		{"promotion page", "promotion_list", map[string]any{"limit": 200}, map[string]any{"limit": 201}},
		{"event page", "events_wait", map[string]any{"limit": 200}, map[string]any{"limit": 201}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t, nil)
			assertSuccess(t, rpc(f, tc.tool, tc.at))
			if w := rpc(f, tc.tool, tc.over); !strings.Contains(w.Body.String(), `"isError":true`) {
				t.Fatalf("N+1 accepted: %s", w.Body.String())
			}
		})
	}
}
func TestPublicArrayBounds(t *testing.T) {
	for _, tc := range []struct {
		tool, key string
		max       int
	}{
		{"run_hold_exec", "argv", 256}, {"events_wait", "repos", 32}, {"events_wait", "refs_glob", 32}, {"events_wait", "kinds", 32}, {"issue_get_many", "ids", 50},
	} {
		t.Run(tc.key, func(t *testing.T) {
			f := setup(t, nil)
			values := []any{}
			var value any = "value"
			switch tc.key {
			case "repos":
				value = "origin/team/widget"
			case "kinds":
				value = "run.updated"
			case "ids":
				value = 1
			}
			for i := 0; i < tc.max; i++ {
				values = append(values, value)
			}
			args := map[string]any{tc.key: values}
			if tc.tool == "run_hold_exec" {
				args["id"] = "12345678"
				args["step"] = "build"
			}
			assertSuccess(t, rpc(f, tc.tool, args))
			args[tc.key] = append(values, value)
			if w := rpc(f, tc.tool, args); !strings.Contains(w.Body.String(), `"isError":true`) {
				t.Fatal(w.Body.String())
			}
		})
	}
}
func TestScopeArrayBounds(t *testing.T) {
	f := setup(t, nil)
	scope := make([]string, 128)
	for i := range scope {
		scope[i] = "widget"
	}
	args := map[string]any{"identity": "agent", "scope": scope}
	assertSuccess(t, rpc(f, "uplink_scope", args))
	args["scope"] = append(scope, "widget")
	if w := rpc(f, "uplink_scope", args); !strings.Contains(w.Body.String(), `"isError":true`) {
		t.Fatal("N+1 accepted")
	}
}

func TestNegativeArgumentLimitsRefuseBeforePreparation(t *testing.T) {
	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"run_list", map[string]any{"limit": -1}}, {"run_get", map[string]any{"id": "12345678", "step_offset": -1}},
		{"run_logs", map[string]any{"id": "12345678", "offset": -1}}, {"run_logs", map[string]any{"id": "12345678", "limit": -1}},
		{"run_logs", map[string]any{"id": "12345678", "byte_offset": -1}}, {"run_logs", map[string]any{"id": "12345678", "context": -1}},
		{"run_wait", map[string]any{"id": "12345678", "timeout_s": -1}}, {"events_wait", map[string]any{"timeout_s": -1}},
	} {
		f := setup(t, nil)
		if w := rpc(f, tc.tool, tc.args); !strings.Contains(w.Body.String(), `"isError":true`) {
			t.Fatalf("%s accepted invalid arguments", tc.tool)
		}
	}
}
