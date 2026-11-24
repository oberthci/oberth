package secretstore

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
)

func TestReadinessAllowsReadableStandbyButRefusesSealedStore(t *testing.T) {
	for _, status := range []int{200, 503, 501} {
		c := statusProbeClient(t, status)
		err := c.Readiness(t.Context())
		if (err == nil) != (status == 200) {
			t.Fatal("wrong read readiness", status, err)
		}
		var refusal *publicerr.Error
		if status == 503 && (!errors.As(err, &refusal) || refusal.Reason() != "Secret store is sealed.") {
			t.Fatal("sealed state lost", err)
		}
		if err != nil && strings.Contains(err.Error(), "PRIVATE-BODY") {
			t.Fatal("health body exposed")
		}
		c.Close()
	}
}

func statusProbeClient(t *testing.T, status int) *Client {
	t.Helper()
	c, err := New(Connection{URL: "https://store.invalid", Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Method != "GET" || r.URL.Path != "/v1/sys/health" || r.URL.Query().Get("standbyok") != "true" || r.URL.Query().Get("perfstandbyok") != "true" || r.Header.Get("X-Vault-Token") != "" {
			t.Fatal("unsafe or incomplete readiness probe")
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("PRIVATE-BODY"))}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	return c
}
