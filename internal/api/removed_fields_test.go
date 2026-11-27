package api_test

import (
	"strings"
	"testing"
)

func TestRemovedSelectorsAreRejected(t *testing.T) {
	f := setup(t, nil)
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"admin_idle", map[string]any{"wait_for": "all"}},
		{"issue_get", map[string]any{"id": 1, "fields": []string{"title"}}},
		{"issue_get_many", map[string]any{"ids": []int{1}, "fields": []string{"title"}}},
	} {
		w := rpc(f, tc.name, tc.args)
		if !strings.Contains(w.Body.String(), "invalid_input") {
			t.Fatal(tc.name, w.Body.String())
		}
	}
	discover(f)
	if w := request(f, "DELETE", "/mcp", nil); w.Code != 405 {
		t.Fatal(w.Code, w.Body.String())
	}
}
