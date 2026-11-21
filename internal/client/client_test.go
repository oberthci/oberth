package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type recorder struct{ *httptest.ResponseRecorder }

func (recorder) SetWriteDeadline(time.Time) error { return nil }

func server(t *testing.T, b apicontract.Bindings, enroll ...apicontract.Handler[model.InviteRedemption, model.InviteCertificate]) http.Handler {
	t.Helper()
	registry, err := api.Builtin(b)
	if err != nil {
		t.Fatal(err)
	}
	clock := fake.NewClock()
	auth := fake.NewAuth(apicontract.Caller{Actor: actor.Actor{Identity: "operator", AuthMethod: "bearer", Admin: true}, CredentialID: "credential"})
	var enrollment apicontract.Handler[model.InviteRedemption, model.InviteCertificate]
	if len(enroll) > 0 {
		enrollment = enroll[0]
	}
	s, err := api.New(api.Config{Enroll: enrollment, Registry: registry, Auth: auth, Runtime: clock.Runtime(), Classify: publicerr.Classify,
		FailureLog: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.BeginDrain)
	return s.Handler()
}

func handlerTransport(h http.Handler) roundTrip {
	return func(r *http.Request) (*http.Response, error) {
		w := recorder{httptest.NewRecorder()}
		h.ServeHTTP(w, r)
		return w.Result(), nil
	}
}

func definition(t *testing.T, name string) apicontract.Definition {
	t.Helper()
	for _, d := range apicontract.Definitions(apicontract.Bindings{}) {
		if d.Name == name {
			return d
		}
	}
	t.Fatal("missing declaration")
	return apicontract.Definition{}
}

func privateFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := WritePrivate(path, data); err != nil {
		t.Fatal(err)
	}
	return path
}

func testClient(t *testing.T, rt http.RoundTripper, wait func(context.Context, time.Duration) error) *Client {
	t.Helper()
	clock := fake.NewClock()
	if wait == nil {
		wait = func(context.Context, time.Duration) error { t.Fatal("unexpected reconnect"); return nil }
	}
	c, err := New(Config{Options: Options{URL: "https://127.0.0.1", TokenFile: privateFile(t, "token", []byte("valid"))}, Transport: rt, Now: clock.Now, Wait: wait})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func TestSafeReadReconnectOnly(t *testing.T) {
	for _, name := range []string{"repo_list", "repo_archive"} {
		t.Run(name, func(t *testing.T) {
			bindings := fake.Bindings()
			h := server(t, bindings)
			base := handlerTransport(h)
			attempts, waits := 0, 0
			rt, deadline := loseFirstReply(t, base, &attempts)
			c := testClient(t, rt, func(ctx context.Context, _ time.Duration) error {
				waits++
				got, _ := ctx.Deadline()
				if got != *deadline {
					t.Fatal("wait deadline changed")
				}
				return nil
			})
			input := json.RawMessage(`{}`)
			if name == "repo_archive" {
				input = json.RawMessage(`{"repo":"project"}`)
			}
			_, refused, err := c.Call(context.Background(), definition(t, name), input)
			assertReconnect(t, bindings, reconnectResult{name, err, refused, attempts, waits})
		})
	}
}

func TestRealTransportJSONSSEAndRefusal(t *testing.T) {
	b := fake.Bindings()
	c := testClient(t, handlerTransport(server(t, b)), nil)
	for _, tc := range []struct{ name, input string }{{"repo_list", `{}`}, {"run_wait", `{"id":"run","timeout_s":1}`}} {
		result, refused, err := c.Call(context.Background(), definition(t, tc.name), json.RawMessage(tc.input))
		if err != nil || refused || !json.Valid(result) {
			t.Fatalf("%s: refused=%v err=%v", tc.name, refused, err)
		}
	}
	result, refused, err := c.Call(context.Background(), definition(t, "run_get"), json.RawMessage(`{}`))
	if err != nil || !refused || !bytes.Contains(result, []byte(`"reason"`)) {
		t.Fatalf("missing refusal: %s %v %v", result, refused, err)
	}
}

func TestReplyBound(t *testing.T) {
	for _, size := range []int{apicontract.RPCReplyBytes(apicontract.ResponseBytes), apicontract.RPCReplyBytes(apicontract.ResponseBytes) + 1} {
		payload := `"` + strings.Repeat("x", size-2) + `"`
		resp := &http.Response{Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(payload))}
		_, err := apicontract.ReadReply(resp, apicontract.RPCReplyBytes(apicontract.ResponseBytes))
		if (err != nil) != (size > apicontract.RPCReplyBytes(apicontract.ResponseBytes)) {
			t.Fatalf("size %d: %v", size, err)
		}
	}
}

func TestReplyProtocolAndSecrets(t *testing.T) {
	for _, payload := range []string{`{}`, `{"jsonrpc":"2.0","id":2,"result":{"structuredContent":{}}}`} {
		data, _, _, err := apicontract.ToolPayload([]byte(payload))
		if err == nil || len(data) != 0 || strings.Contains(err.Error(), "do-not-print") {
			t.Fatal("unsafe protocol or secret reply")
		}
	}
}

func TestReconnectCancellationAndClassification(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cause   error
		retries bool
	}{{"lost", io.ErrUnexpectedEOF, true}, {"trust", errors.New("trust refused"), false}, {"cancel", context.Canceled, false}} {
		t.Run(tc.name, func(t *testing.T) {
			attempts := 0
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c := testClient(t, roundTrip(func(*http.Request) (*http.Response, error) { attempts++; return nil, tc.cause }), func(context.Context, time.Duration) error { cancel(); return ctx.Err() })
			_, _, err := c.Call(ctx, definition(t, "repo_list"), json.RawMessage(`{}`))
			if err == nil || attempts != 1 {
				t.Fatal("unexpected replay")
			}
			if tc.retries && !errors.Is(err, context.Canceled) {
				t.Fatal("cancellation lost")
			}
		})
	}
}

func TestMessageCatalogue(t *testing.T) {
	all := catalogue()
	for key, m := range fileMessages() {
		all["file_"+key] = m
	}
	for key, m := range all {
		if publicerr.CheckMessage(m[0], 80) != nil || publicerr.CheckMessage(m[1], 120) != nil {
			t.Fatalf("invalid catalogue: %s", key)
		}
	}
}

func loseFirstReply(t *testing.T, base http.RoundTripper, attempts *int) (http.RoundTripper, *time.Time) {
	t.Helper()
	var deadline time.Time
	rt := roundTrip(func(r *http.Request) (*http.Response, error) {
		got, ok := r.Context().Deadline()
		if !ok {
			t.Fatal("missing deadline")
		}
		if *attempts == 0 {
			deadline = got
		} else if got != deadline {
			t.Fatal("deadline renewed")
		}
		*attempts++
		resp, err := base.RoundTrip(r)
		if err != nil {
			return nil, err
		}
		if *attempts == 1 {
			_ = resp.Body.Close()
			return nil, io.EOF
		}
		return resp, nil
	})
	return rt, &deadline
}

type reconnectResult struct {
	name            string
	err             error
	refused         bool
	attempts, waits int
}

func assertReconnect(t *testing.T, bindings apicontract.Bindings, result reconnectResult) {
	t.Helper()
	name, err, refused, attempts, waits := result.name, result.err, result.refused, result.attempts, result.waits

	if name == "repo_archive" {
		_, _, calls := bindings.RepoArchive.Handler.(*fake.Handler[apicontract.RepoArchiveArgs, apicontract.Repository]).Observed()
		if err == nil || calls != 1 || attempts != 1 || waits != 0 {
			t.Fatalf("mutation replay: calls=%d attempts=%d waits=%d err=%v", calls, attempts, waits, err)
		}
	} else {
		_, _, calls := bindings.RepoList.Handler.(*fake.Handler[apicontract.RepoListArgs, apicontract.RepoPage]).Observed()
		if err != nil || refused || calls != 2 || attempts != 2 || waits != 1 {
			t.Fatalf("read reconnect: calls=%d attempts=%d waits=%d err=%v", calls, attempts, waits, err)
		}
	}
}

func TestToolDeadlineAndRemainingPoll(t *testing.T) {
	d := definition(t, "run_wait")
	if got := callTimeout(d, json.RawMessage(`{}`)); got != 630*time.Second {
		t.Fatalf("default deadline %s", got)
	}
	if got := callTimeout(definition(t, "run_hold_exec"), json.RawMessage(`{}`)); got != 930*time.Second {
		t.Fatalf("tool deadline %s", got)
	}
	input := remainingInput(d, json.RawMessage(`{"id":"run","timeout_s":600}`), 45*time.Second)
	var args apicontract.RunWaitArgs
	if err := json.Unmarshal(input, &args); err != nil || args.TimeoutS != 15 {
		t.Fatalf("poll extended: %s", input)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := testClient(t, roundTrip(func(r *http.Request) (*http.Response, error) { return nil, r.Context().Err() }), nil)
	if _, _, err := c.Call(ctx, d, json.RawMessage(`{"id":"run"}`)); !errors.Is(err, context.Canceled) {
		t.Fatal("caller cancellation lost")
	}
}

func TestTransientHTTPKeepsServerRefusal(t *testing.T) {
	b := fake.Bindings()
	h := server(t, b)
	attempts := 0
	c := testClient(t, roundTrip(func(r *http.Request) (*http.Response, error) {
		attempts++
		resp, err := handlerTransport(h).RoundTrip(r)
		if attempts == 1 {
			resp.StatusCode = 503
			resp.Body = io.NopCloser(strings.NewReader(`{"code":"unavailable","reason":"Server is restarting.","remedy":"Reconnect and read durable state."}`))
		}
		return resp, err
	}), func(context.Context, time.Duration) error { return nil })
	data, refused, err := c.Call(context.Background(), definition(t, "repo_list"), json.RawMessage(`{}`))
	if err != nil || refused || attempts != 2 || !json.Valid(data) {
		t.Fatal("HTTP read reconnect failed")
	}
}

func TestSSEBound(t *testing.T) {
	for _, size := range []int{apicontract.RPCReplyBytes(apicontract.ResponseBytes), apicontract.RPCReplyBytes(apicontract.ResponseBytes) + 1} {
		prefix := `data: {"jsonrpc":"2.0","id":1,"result":"`
		suffix := "\"}\n\n"
		body := prefix + strings.Repeat("x", size-len(prefix)-len(suffix)) + suffix
		resp := &http.Response{Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}
		_, err := apicontract.ReadReply(resp, apicontract.RPCReplyBytes(apicontract.ResponseBytes))
		if (err != nil) != (size > apicontract.RPCReplyBytes(apicontract.ResponseBytes)) {
			t.Fatalf("SSE size %d: %v", size, err)
		}
	}
}
