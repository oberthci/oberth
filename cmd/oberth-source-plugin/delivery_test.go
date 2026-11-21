package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/sourcedelivery"
)

func prepared(t *testing.T, b sourcecontract.Binding) *admissioncontract.PreparedAdmission {
	t.Helper()
	p, err := admissioncontract.Seal(admissioncontract.PreparedView{Workflow: &wfv1.Workflow{}, RunID: "run-1", Repository: b.Revision.Repository, SourceBindings: []sourcecontract.Binding{b}})
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func load(t *testing.T, s http.Handler, request sourcedelivery.LoadRequest, header string) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, sourcedelivery.LoadPath, bytes.NewReader(b))
	r.Header.Set("Authorization", header)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}
func instant() time.Time { return time.Unix(100, 0) }

// Covers: REQ-C6-010
func TestExactRecordedWorkflowSourceContract(t *testing.T) {
	f := gittest.New(t)
	cache := f.Cache(t, nil)
	binding := f.Binding()
	p := prepared(t, binding)
	caller := sourcedelivery.Caller{Namespace: "runs", Workflow: "workflow", UID: "uid-1"}
	run := sourcedelivery.Run{Admission: p.View(), Namespace: "runs", Workflow: "workflow", UID: "uid-1"}
	server := sourcedelivery.New(sourcedelivery.Config{Now: instant, SourceClientBurst: 30,
		Authenticate: func(_ context.Context, token string) (sourcedelivery.Caller, error) {
			if token != "valid" {
				return sourcedelivery.Caller{}, &sourcedelivery.Failure{Code: "unauthenticated"}
			}
			return caller, nil
		},
		Run: func(context.Context, string) (sourcedelivery.Run, error) { return run, nil }, Stage: cache.Stage})
	request := sourcedelivery.LoadRequest{Locator: "run-1", Descriptor: binding.Descriptor}
	w := load(t, server, request, "Bearer valid")
	if w.Code != 200 {
		t.Fatalf("status %d %s", w.Code, w.Body.String())
	}
	response := w.Result()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	digest := sha256.Sum256(body)
	if err = sourcedelivery.VerifyReceipt(response.Trailer.Get(sourcedelivery.ReceiptTrailer), request, hex.EncodeToString(digest[:])); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func()
		undo   func()
		status int
	}{
		{"workflow name", func() { caller.Workflow = "replacement" }, func() { caller.Workflow = "workflow" }, 403},
		{"workflow UID", func() { caller.UID = "uid-2" }, func() { caller.UID = "uid-1" }, 403},
		{"namespace", func() { caller.Namespace = "other" }, func() { caller.Namespace = "runs" }, 403},
		{"run", func() { request.Locator = "run-2" }, func() { request.Locator = "run-1" }, 403},
		{"descriptor", func() {
			request.Descriptor = sourcecontract.Descriptor{Version: 1, Kind: sourcecontract.Pinned, Repository: gittest.Repository, Commit: f.Commit}
		}, func() { request.Descriptor = binding.Descriptor }, 403},
		{"terminal", func() { run.Terminal = true }, func() { run.Terminal = false }, 403},
		{"UID pending", func() { run.UID = "" }, func() { run.UID = "uid-1" }, 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.change()
			defer tc.undo()
			got := load(t, server, request, "Bearer valid")
			if got.Code != tc.status {
				t.Fatalf("%d: %s", got.Code, got.Body.String())
			}
			if tc.status == 503 && got.Header().Get("Retry-After") != "10" {
				t.Fatal(got.Header())
			}
		})
	}
	if got := load(t, server, request, "Bearer invalid"); got.Code != 401 {
		t.Fatal(got.Code)
	}
}
func TestPinDescriptorCannotChangeCommit(t *testing.T) {
	f := gittest.New(t)
	cache := f.Cache(t, nil)
	b := f.Binding()
	b.Path = "/work/sources/pin"
	b.Export = sourcecontract.Export{}
	b.AuthorizationDigest = strings.Repeat("a", 64)
	b.Descriptor = sourcecontract.Descriptor{Version: 1, Kind: sourcecontract.Pinned, Repository: gittest.Repository, Commit: f.Commit}
	p := prepared(t, b)
	caller := sourcedelivery.Caller{Namespace: "ns", Workflow: "wf", UID: "uid"}
	server := sourcedelivery.New(sourcedelivery.Config{Now: instant, SourceClientBurst: 9, Authenticate: func(context.Context, string) (sourcedelivery.Caller, error) { return caller, nil }, Run: func(context.Context, string) (sourcedelivery.Run, error) {
		return sourcedelivery.Run{Admission: p.View(), Namespace: "ns", Workflow: "wf", UID: "uid"}, nil
	}, Stage: cache.Stage})
	request := sourcedelivery.LoadRequest{Locator: "run-1", Descriptor: b.Descriptor}
	if got := load(t, server, request, "Bearer valid"); got.Code != 200 {
		t.Fatal(got.Code)
	}
	request.Descriptor.Repository = "origin/team/other"
	if got := load(t, server, request, "Bearer valid"); got.Code != 403 {
		t.Fatal(got.Code)
	}
	request.Descriptor.Repository = gittest.Repository
	request.Descriptor.Commit = strings.Repeat("a", 40)
	if got := load(t, server, request, "Bearer valid"); got.Code != 403 {
		t.Fatal(got.Code)
	}
}
func TestWireRefusesUnknownTrailingNullAndOversize(t *testing.T) {
	valid := `{"locator":"run-1","descriptor":{"version":1,"kind":"main"}}`
	for _, body := range []string{`null`, `{}`, valid + ` {}`, strings.Replace(valid, `"locator"`, `"unknown"`, 1), strings.Replace(valid, `"version":1`, `"version":null`, 1), strings.Repeat(" ", sourcedelivery.MaxLoadRequestBytes) + valid} {
		if _, err := sourcedelivery.DecodeRequest([]byte(body)); err == nil {
			t.Fatalf("accepted %q", body[:min(len(body), 80)])
		}
	}
	padded := valid + strings.Repeat(" ", sourcedelivery.MaxLoadRequestBytes-len(valid))
	if _, err := sourcedelivery.DecodeRequest([]byte(padded)); err != nil {
		t.Fatal(err)
	}
	if _, err := sourcedelivery.DecodeRequest([]byte(padded + " ")); err == nil {
		t.Fatal("N+1 accepted")
	}
}
func TestEndpointMethodsAndUnconfigured(t *testing.T) {
	server := sourcedelivery.New(sourcedelivery.Config{})
	for _, method := range []string{"GET", "PUT", "DELETE"} {
		w := httptest.NewRecorder()
		server.ServeHTTP(w, httptest.NewRequest(method, sourcedelivery.LoadPath, nil))
		if w.Code != 405 {
			t.Fatal(w.Code)
		}
	}
	w := httptest.NewRecorder()
	server.ServeHTTP(w, httptest.NewRequest("POST", sourcedelivery.LoadPath, nil))
	if w.Code != 501 {
		t.Fatal(w.Code)
	}
}
