package api_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/publicerr"
)

func TestSecretTransportConsumesOnlyPrivateBodyAndNeverLogsIt(t *testing.T) {
	var logs bytes.Buffer
	marker := []byte("private-body-marker")
	calls := 0
	f := setup(t, func(c *api.Config, b *apicontract.Bindings) {
		c.FailureLog = slog.New(slog.NewTextHandler(&logs, nil))
		b.SecretPut.Handler = apicontract.HandlerFunc[apicontract.SecretPutArgs, apicontract.Receipt](func(ctx context.Context, _ apicontract.Caller, q apicontract.SecretPutArgs) (apicontract.Call[apicontract.Receipt], error) {
			value, ok := apicontract.SecretValue(ctx)
			if !ok || !bytes.Equal(value, marker) || q.Secret != "key" {
				t.Fatal("private body missing")
			}
			return func(context.Context) (apicontract.Receipt, error) {
				calls++
				return apicontract.Receipt{State: "stored"}, nil
			}, nil
		})
	})
	body := apicontract.SecretPutBody{SecretPutArgs: apicontract.SecretPutArgs{Repo: "origin/team/widget", Workflow: ".oberth/build.yaml", Step: "build", Secret: "key"}, Value: marker}
	raw, _ := json.Marshal(body)
	w := request(f, "POST", "/api/secrets/put", raw)
	assertSuccess(t, w)
	if calls != 1 || bytes.Contains(w.Body.Bytes(), marker) || bytes.Contains(logs.Bytes(), marker) || bytes.Contains(logs.Bytes(), []byte(base64.StdEncoding.EncodeToString(marker))) {
		t.Fatal("private value leaked")
	}

}

func TestSecretTransportAuthorizationAndBodyLimit(t *testing.T) {
	f := setup(t, nil)
	for _, tc := range []struct {
		method, target string
		body           []byte
	}{
		{"GET", "/api/secrets/put?value=private", nil},
	} {
		if w := request(f, tc.method, tc.target, tc.body); w.Code == 200 {
			t.Fatal("unbounded or query secret accepted")
		}
	}
	body := []byte(`{"repo":"origin/team/widget","workflow":"build.yaml","step":"build","secret":"key","value":"eA=="}`)
	body = append(body, bytes.Repeat([]byte(" "), apicontract.SecretBodyBytes+1-len(body))...)
	if !json.Valid(body) || len(body) != apicontract.SecretBodyBytes+1 {
		t.Fatal("invalid oversized fixture")
	}
	if w := request(f, "POST", "/api/secrets/put", body); w.Code != 413 {
		t.Fatal("valid oversized JSON did not hit the body bound", w.Code)
	}
	c := admin()
	c.Actor.Admin = false
	f.auth.Set(c, nil)
	w := request(f, "POST", "/api/secrets/put", []byte(`{"repo":"origin/team/widget","workflow":"build.yaml","step":"build","secret":"key","value":"eA=="}`))
	if w.Code != 403 {
		t.Fatal("non-admin body accepted", w.Code)
	}
}

func TestSecretTransportPublicRefusalDoesNotWriteFailureLog(t *testing.T) {
	var logs bytes.Buffer
	f := setup(t, func(c *api.Config, b *apicontract.Bindings) {
		c.FailureLog = slog.New(slog.NewTextHandler(&logs, nil))
		b.SecretPut.Handler = apicontract.HandlerFunc[apicontract.SecretPutArgs, apicontract.Receipt](func(context.Context, apicontract.Caller, apicontract.SecretPutArgs) (apicontract.Call[apicontract.Receipt], error) {
			return nil, publicerr.New(publicerr.Unavailable, "Secret store is sealed.", "An administrator runs oberth unseal, then retry.", nil)
		})
	})
	w := request(f, "POST", "/api/secrets/put", []byte(`{"repo":"origin/team/widget","workflow":"build.yaml","step":"build","secret":"key","value":"cHJpdmF0ZS1ib2R5LW1hcmtlcg=="}`))
	if w.Code != 503 || !strings.Contains(w.Body.String(), "oberth unseal") || logs.Len() != 0 {
		t.Fatal("sealed refusal differs")
	}
}

func TestSecretTransportRefusesQueriesAndMalformedBody(t *testing.T) {
	var logs bytes.Buffer
	marker := []byte("private-body-marker")
	f := setup(t, func(c *api.Config, b *apicontract.Bindings) {
		c.FailureLog = slog.New(slog.NewTextHandler(&logs, nil))
		b.SecretPut.Handler = apicontract.HandlerFunc[apicontract.SecretPutArgs, apicontract.Receipt](func(context.Context, apicontract.Caller, apicontract.SecretPutArgs) (apicontract.Call[apicontract.Receipt], error) {
			t.Fatal("invalid body reached writer")
			return nil, nil
		})
	})
	raw := []byte(`{"repo":"origin/team/widget","workflow":"build.yaml","step":"build","secret":"key","value":"eA=="}`)
	for _, target := range []string{"/api/secrets/put?value=private-body-marker", "/api/secrets/put?secret=key"} {
		w := request(f, "POST", target, raw)
		if w.Code == 200 || bytes.Contains(w.Body.Bytes(), marker) {
			t.Fatal("query accepted or echoed")
		}
	}
	w := request(f, "POST", "/api/secrets/put", []byte(`{"value":"private-body-marker"`))
	if w.Code == 200 || bytes.Contains(w.Body.Bytes(), marker) || bytes.Contains(logs.Bytes(), marker) || bytes.Contains(logs.Bytes(), []byte(base64.StdEncoding.EncodeToString(marker))) {
		t.Fatal("malformed body leaked or reached writer")
	}
}

func TestSecretTransportRequiresAnExplicitValue(t *testing.T) {
	f := setup(t, func(_ *api.Config, b *apicontract.Bindings) {
		b.SecretPut.Handler = apicontract.HandlerFunc[apicontract.SecretPutArgs, apicontract.Receipt](func(context.Context, apicontract.Caller, apicontract.SecretPutArgs) (apicontract.Call[apicontract.Receipt], error) {
			t.Fatal("absent value reached writer")
			return nil, nil
		})
	})
	for _, value := range []string{"", `,"value":null`} {
		body := []byte(`{"repo":"origin/team/widget","workflow":"build.yaml","step":"build","secret":"key"` + value + `}`)
		if w := request(f, "POST", "/api/secrets/put", body); w.Code != 400 {
			t.Fatal("missing private value accepted", w.Code)
		}
	}
}
