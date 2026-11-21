package client

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
)

func TestPrivateSecretWriteRouteAndNoReplay(t *testing.T) {
	args := apicontract.SecretPutArgs{Repo: "origin/team/widget", Workflow: ".oberth/build.yaml", Step: "build", Secret: "registry/key"}
	for _, status := range []int{200, 403, 503, 302} {
		calls := 0
		c := testClient(t, roundTrip(func(r *http.Request) (*http.Response, error) {
			calls++
			checkSecretRequest(t, r, args)
			reply := `{"id":"registry/key","state":"stored"}`
			if status != 200 {
				reply = `{"code":"forbidden","reason":"Secret write refused.","remedy":"Check the approved grant."}`
			}
			return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(reply))}, nil
		}), nil)
		receipt, err := c.PutSecret(t.Context(), args, []byte("private-body-marker"))
		if calls != 1 || (err == nil) != (status == 200) {
			t.Fatal("private write result or replay", status, calls, err)
		}
		if status == 200 && receipt.ID != args.Secret {
			t.Fatal("public receipt path changed")
		}
	}
}
func TestPrivateSecretWriteBoundsBeforeTransport(t *testing.T) {
	c := testClient(t, roundTrip(func(*http.Request) (*http.Response, error) {
		t.Fatal("oversize value reached transport")
		return nil, nil
	}), nil)
	if _, err := c.PutSecret(t.Context(), apicontract.SecretPutArgs{}, make([]byte, model.MaxSecretValueBytes+1)); err == nil {
		t.Fatal("oversize value accepted")
	}
}

func checkSecretRequest(t *testing.T, r *http.Request, args apicontract.SecretPutArgs) {
	t.Helper()
	var body apicontract.SecretPutBody
	if r.Method != "POST" || r.URL.Path != "/api/secrets/put" || r.URL.RawQuery != "" || r.Header.Get("Content-Type") != "application/json" {
		t.Fatal("incorrect private secret route")
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil || body.SecretPutArgs != args || !bytes.Equal(body.Value, []byte("private-body-marker")) {
		t.Fatal("private secret body changed")
	}
}
