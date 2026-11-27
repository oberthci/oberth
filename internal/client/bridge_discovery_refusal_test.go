package client

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/publicerr"
)

func TestBridgeDiscoveryKeepsPublicRefusals(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(map[bool]string{false: "authentication", true: "restart"}[restart], func(t *testing.T) {
			registry, err := api.Builtin(fake.Bindings())
			if err != nil {
				t.Fatal(err)
			}
			s, err := api.New(api.Config{Registry: registry, Auth: fake.NewAuth(apicontract.Caller{}),
				Runtime: fake.NewClock().Runtime(), Classify: publicerr.Classify,
				FailureLog: slog.New(slog.NewTextHandler(io.Discard, nil))})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(s.BeginDrain)
			reason, remedy, status := "Caller credentials are invalid.", "Reconnect with a current bearer or verified client certificate.", 401
			if restart {
				s.BeginDrain()
				reason, remedy, status = "Server is restarting.", "Reconnect and inspect durable state before repeating an action.", 503
			}
			p := newBridgePeer(t, bridgeProfile(t, roundTrip(func(r *http.Request) (*http.Response, error) {
				r.Header.Set("Authorization", "Bearer invalid-synthetic")
				resp, sendErr := handlerTransport(s.Handler()).RoundTrip(r)
				if resp.StatusCode != status {
					t.Errorf("public refusal status = %d, want %d", resp.StatusCode, status)
				}
				return resp, sendErr
			})))
			checkBridgeCatalogueRefusals(t, p, reason, remedy)
		})
	}
}

func checkBridgeCatalogueRefusals(t *testing.T, p *bridgePeer, reason, remedy string) {
	t.Helper()
	for _, method := range []string{"server/discover", "tools/list", "skills/list", "skills/get", "resources/list", "resources/read"} {
		p.send(method, method, nil)
		var reply struct {
			ID    string
			Error struct {
				Code    int
				Message string
				Data    struct{ Reason, Remedy string }
			}
		}
		data := p.frame()
		if json.Unmarshal(data, &reply) != nil || reply.ID != method || reply.Error.Code != -32000 || reply.Error.Message != reason || reply.Error.Data.Reason != reason || reply.Error.Data.Remedy != remedy {
			t.Fatalf("lost public refusal: %s", data)
		}
	}
}

func TestBridgeDiscoveryRejectsInvalidReplies(t *testing.T) {
	for _, body := range []string{
		`not json`, `null`, `[]`, `{}`,
		`{"jsonrpc":"2.0","id":"other","error":{"code":-32601}}`,
		`{"jsonrpc":"1.0","id":"discovery","result":{}}`,
		`{"jsonrpc":"2.0","id":"discovery"}`,
		`{"jsonrpc":"2.0","id":"discovery","result":null}`,
		`{"jsonrpc":"2.0","id":"discovery","result":[]}`,
		`{"jsonrpc":"2.0","id":"discovery","result":{},"error":{"code":-32601}}`,
		`{"code":"forbidden","reason":"Only a reason."}`,
		`{"jsonrpc":"","id":"discovery","code":"forbidden","reason":"Refused.","remedy":"Reconnect."}`,
		`{"jsonrpc":"2.0","id":"other","code":"forbidden","reason":"Refused.","remedy":"Reconnect."}`,
	} {
		for _, status := range []int{200, 503} {
			p := newBridgePeer(t, bridgeProfile(t, roundTrip(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewBufferString(body)), Request: r}, nil
			})))
			p.send("discovery", "server/discover", nil)
			data := p.frame()
			if !bytes.Contains(data, []byte(`"message":"Server reply is invalid."`)) {
				t.Fatalf("accepted HTTP %d body %s: %s", status, body, data)
			}
		}
	}
}
