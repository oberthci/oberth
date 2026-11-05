package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/storetest"
)

type cliConcurrencyObserver struct{ groups []model.AdmissionGroup }

func (*cliConcurrencyObserver) AdmissionSnapshot(int64) *model.AdmissionObservation { return nil }
func (o *cliConcurrencyObserver) ConcurrencyGroups(context.Context) ([]model.AdmissionGroup, error) {
	return o.groups, nil
}

// Only admission data is supplied by a fixture. The service, authenticated MCP
// handler, converter, TLS transport and operator CLI are their real entrypoints.
func TestAdmissionGroupsRealMCPResult(t *testing.T) {
	for _, populated := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty", true: "populated"}[populated], func(t *testing.T) {
			db, err := storetest.Open(t.Context(), filepath.Join(t.TempDir(), "state.sqlite"), store.Options{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			groups := []model.AdmissionGroup{}
			if populated {
				groups = append(groups, model.AdmissionGroup{Name: "shared-state", Limit: 2, Holders: []string{"holder-a", "holder-b"}, Waiting: []string{"waiter-a"}})
			}
			control, err := service.NewAPI(service.APIConfig{Runs: db, Admission: &cliConcurrencyObserver{groups: groups}})
			if err != nil {
				t.Fatal(err)
			}
			server, err := api.New(toolingAuthenticator{}, control, control, "test", api.WithErrorClassifier(classifyViewError))
			if err != nil {
				t.Fatal(err)
			}
			httpServer := httptest.NewTLSServer(server.Handler())
			defer httpServer.Close()
			defer withTestMCPClient(t, httpServer)()
			args := []string{"groups", "--url", httpServer.URL + "/mcp", "--token-file", structuredMCPTokenFile(t)}
			var table strings.Builder
			if err := runAdmission(t.Context(), args, &table); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(table.String(), "GROUP") || (populated && (!strings.Contains(table.String(), "shared-state") || !strings.Contains(table.String(), "holder-a, holder-b") || !strings.Contains(table.String(), "waiter-a"))) {
				t.Fatalf("incomplete group table: %q", table.String())
			}
			var output strings.Builder
			if err := runAdmission(t.Context(), append(args, "--json"), &output); err != nil {
				t.Fatal(err)
			}
			var response struct {
				Groups []model.AdmissionGroup `json:"groups"`
			}
			if err := json.Unmarshal([]byte(output.String()), &response); err != nil || response.Groups == nil || !reflect.DeepEqual(response.Groups, groups) {
				t.Fatalf("incorrect JSON: %q (%v)", output.String(), err)
			}
			output.Reset()
			if err := runMCPCall(t.Context(), []string{"concurrency_groups", "--url", httpServer.URL + "/mcp", "--token-file", structuredMCPTokenFile(t)}, &output); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(output.String()), &response); err != nil || !reflect.DeepEqual(response.Groups, groups) {
				t.Fatalf("generic MCP result differs: %q (%v)", output.String(), err)
			}
		})
	}
}

func TestAdmissionGroupsMalformedAndLegacyResults(t *testing.T) {
	for _, body := range []string{`{}`, `{"groups":null}`, `{"groups":{}}`, `{"groups":7}`, `{"groups":["bad"]}`, `null`, `not-json`} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request jsonRPCRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					return
				}
				testWriteJSONRPCResult(w, &request.ID, toolsCallResult{Content: []mcpContent{{Type: "text", Text: body}}})
			}))
			defer server.Close()
			defer withTestMCPClient(t, server)()
			for _, asJSON := range []bool{false, true} {
				args := []string{"groups", "--url", server.URL, "--token-file", structuredMCPTokenFile(t)}
				if asJSON {
					args = append(args, "--json")
				}
				var out strings.Builder
				if err := runAdmission(t.Context(), args, &out); err == nil || out.Len() != 0 {
					t.Fatalf("malformed groups accepted: %q, %v, %q", body, err, out.String())
				}
			}
		})
	}
	for _, body := range []string{`[]`, `[{"name":"legacy","limit":1,"holders":["holder"]}]`} {
		t.Run("legacy-"+body, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request jsonRPCRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					return
				}
				testWriteJSONRPCResult(w, &request.ID, toolsCallResult{Content: []mcpContent{{Type: "text", Text: body}}})
			}))
			defer server.Close()
			defer withTestMCPClient(t, server)()
			var out strings.Builder
			if err := runAdmission(t.Context(), []string{"groups", "--url", server.URL, "--token-file", structuredMCPTokenFile(t)}, &out); err != nil || !strings.Contains(out.String(), "GROUP") {
				t.Fatal(out.String(), err)
			}
			out.Reset()
			if err := runAdmission(t.Context(), []string{"groups", "--json", "--url", server.URL, "--token-file", structuredMCPTokenFile(t)}, &out); err != nil || out.String() != body+"\n" {
				t.Fatalf("legacy JSON passthrough changed: %q, %v", out.String(), err)
			}
		})
	}
	t.Run("tool-error", func(t *testing.T) {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var request jsonRPCRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
				return
			}
			testWriteJSONRPCResult(w, &request.ID, toolsCallResult{IsError: true, Content: []mcpContent{{Type: "text", Text: "refused"}}})
		}))
		defer server.Close()
		defer withTestMCPClient(t, server)()
		var out strings.Builder
		if err := runAdmission(t.Context(), []string{"groups", "--url", server.URL, "--token-file", structuredMCPTokenFile(t)}, &out); !errors.Is(err, errMCPToolError) || out.Len() != 0 {
			t.Fatal(out.String(), err)
		}
	})
}
