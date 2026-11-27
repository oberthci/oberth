package api_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/oberthci/oberth/internal/apicontract"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/runlog"
)

func TestEachToolHasItsOwnDescribedInput(t *testing.T) {
	typ := reflect.TypeFor[apicontract.Bindings]()
	seen := map[reflect.Type]string{}
	for _, field := range reflect.VisibleFields(typ) {
		if field.Anonymous {
			continue
		}
		slot := field.Type.Field(0).Type
		input := slot.Method(0).Type.In(2)
		if prev, ok := seen[input]; ok {
			t.Fatalf("%s shares input with %s", field.Name, prev)
		}
		seen[input] = field.Name
	}
	r, err := api.Builtin(fake.Bindings())
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range listTools(t, r, admin()) {
		if len(strings.Fields(m.Description)) < 10 || strings.EqualFold(strings.TrimSuffix(m.Description, "."), strings.ReplaceAll(m.Name, "_", " ")) {
			t.Fatalf("guidance absent: %s", m.Name)
		}
		describedFields(t, m.InputSchema)
	}
	f := setup(t, nil)
	w := discover(f)
	if !strings.Contains(w.Body.String(), `"instructions":`) || !strings.Contains(w.Body.String(), "publication_list") {
		t.Fatal("agent instructions absent")
	}
}
func describedFields(t *testing.T, s apicontract.Schema) {
	t.Helper()
	if variants, ok := s["anyOf"].([]apicontract.Schema); ok {
		s = variants[0]
	}
	if item, ok := s["items"].(apicontract.Schema); ok {
		describedFields(t, item)
	}
	props, _ := s["properties"].(apicontract.Schema)
	for key, v := range props {
		field := v.(apicontract.Schema)
		desc, _ := field["description"].(string)
		if len(strings.Fields(desc)) < 5 || strings.EqualFold(desc, key) {
			t.Fatalf("field guidance absent: %s", key)
		}
		describedFields(t, field)
	}
}
func TestExactSchemasAndNestedRequiredFields(t *testing.T) {
	f := setup(t, nil)
	for _, tc := range []struct {
		name string
		args any
	}{
		{"run_cancel", map[string]any{"id": "12345678", "assist": true}},
		{"schedule_list", map[string]any{"force": true}},
		{"promote_status", map[string]any{"id": "12345678", "repo": "widget"}},
		{"uplink_scope", map[string]any{"identity": "agent", "bearer": true, "scope": []string{}}},
		{"repo_add", map[string]any{"repo": "widget"}},
		{"upstream_add", map[string]any{"name": "origin"}},
		{"uplink_rotate", map[string]any{"fingerprint": "old"}},
		{"access_dismiss", map[string]any{}},
		{"publication_abandon", map[string]any{"id": "12345678"}},
		{"run_purge", map[string]any{"id": "12345678", "what": "logs"}},
		{"run_check", map[string]any{"repo": "widget", "files": []any{map[string]any{"path": "a.yaml"}}}},
		{"workflow_capability_grant", map[string]any{"repo": "widget", "capability": map[string]any{"namespace": "execution"}}},
	} {
		if w := rpc(f, tc.name, tc.args); !strings.Contains(w.Body.String(), `"isError":true`) {
			t.Fatalf("%s accepted wrong shape: %s", tc.name, w.Body.String())
		}
	}
	assertSuccess(t, rpc(f, "uplink_scope", map[string]any{"identity": "agent", "scope": []string{}}))
}
func TestRepositorySyntaxStopsBeforeService(t *testing.T) {
	f := setup(t, nil)
	h := f.bindings.RunList.Handler.(*fake.Handler[apicontract.RunListArgs, apicontract.RunPage])
	for _, repo := range []string{"../widget", "origin//widget", "/widget", "a/b/c/d"} {
		if w := rpc(f, "run_list", map[string]any{"repo": repo}); !strings.Contains(w.Body.String(), `"isError":true`) {
			t.Fatalf("accepted repo %q", repo)
		}
	}
	if inputs, _, _ := h.Observed(); len(inputs) != 0 {
		t.Fatal("invalid repository reached service")
	}
	assertSuccess(t, rpc(f, "run_list", map[string]any{"repo": "origin/team/widget"}))
}
func TestServiceOwnsCombinationsAndCompleteResults(t *testing.T) {
	f := setup(t, nil)
	for _, tc := range []struct {
		name string
		args any
	}{
		{"wait", map[string]any{"id": "12345678", "sha": "abcd", "ref": "main"}},
		{"promote", map[string]any{"branch": "main", "sha": "abcd"}},
		{"run_list", map[string]any{"held": true, "latest": true}},
		{"run_check", map[string]any{"repo": "widget", "files": []any{map[string]string{"path": "file.txt", "content": "contents"}}}},
	} {
		assertSuccess(t, rpc(f, tc.name, tc.args))
	}
}
func TestWaitBoundaryHasMarginAndClampsServiceTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		parent, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		outer, _ := parent.Deadline()
		var input apicontract.RunWaitArgs
		var deadline time.Time
		f := setup(t, func(c *api.Config, b *apicontract.Bindings) {
			c.MaximumWait = time.Second
			b.RunWait.Handler = apicontract.HandlerFunc[apicontract.RunWaitArgs, apicontract.WaitResult](func(ctx context.Context, _ apicontract.Caller, a apicontract.RunWaitArgs) (apicontract.Call[apicontract.WaitResult], error) {
				input = a
				deadline, _ = ctx.Deadline()
				return func(ctx context.Context) (apicontract.WaitResult, error) {
					timer := time.NewTimer(time.Duration(a.TimeoutS) * time.Second)
					defer timer.Stop()
					select {
					case <-timer.C:
						return apicontract.WaitResult{TimedOut: true}, nil
					case <-ctx.Done():
						return apicontract.WaitResult{}, ctx.Err()
					}
				}, nil
			})
		})
		r := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"run_wait","arguments":{"id":"12345678","timeout_s":600}}}`)).WithContext(parent)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer valid")
		w := httptest.NewRecorder()
		f.server.Handler().ServeHTTP(w, modernRequest(r))
		assertSuccess(t, w)
		if input.TimeoutS != 1 || outer.Sub(deadline) != 4*time.Second || !strings.Contains(w.Body.String(), `"timed_out":true`) {
			t.Fatal("wait boundary lost its timeout or deadline margin", w.Body.String(), input, outer.Sub(deadline))
		}
	})
}
func TestExpiredWaitDeadlineIsStillATimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := setup(t, func(c *api.Config, b *apicontract.Bindings) {
			c.MaximumWait = time.Second
			b.RunWait.Handler = &fake.Handler[apicontract.RunWaitArgs, apicontract.WaitResult]{Run: func(ctx context.Context) (apicontract.WaitResult, error) {
				<-ctx.Done()
				return apicontract.WaitResult{}, ctx.Err()
			}}
		})
		w := rpc(f, "run_wait", map[string]any{"id": "12345678"})
		assertSuccess(t, w)
		if !strings.Contains(w.Body.String(), `"timed_out":true`) {
			t.Fatal(w.Body.String())
		}
	})
}
func TestLongStatusReasonDoesNotEraseReply(t *testing.T) {
	f := setup(t, func(_ *api.Config, b *apicontract.Bindings) {
		b.RunLogs.Handler = &fake.Handler[apicontract.RunLogsArgs, apicontract.LogResult]{Value: apicontract.LogResult{Content: strings.Repeat("evidence", 20), Meta: runlog.Meta{Reason: strings.Repeat("r", 41)}}}
	})
	w := rpc(f, "run_logs", map[string]any{"id": "12345678"})
	assertSuccess(t, w)
	if !strings.Contains(w.Body.String(), strings.Repeat("r", 41)) {
		t.Fatal("producer reason erased reply")
	}
	var wire map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &wire); err != nil {
		t.Fatal(err)
	}
}
