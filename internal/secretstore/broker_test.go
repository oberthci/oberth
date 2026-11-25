package secretstore

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
)

func TestBrokerRefusalsPreserveRecoveryAndRejectMissingValues(t *testing.T) {
	for _, test := range []struct {
		status       int
		body, reason string
	}{
		{502, `{"private":"reply"}`, "Secret delivery is unavailable."},
		{503, `{"private":"reply"}`, "Secret store is sealed."},
		{404, `{"private":"reply"}`, "No value is stored for this path."},
		{200, `{}`, "Secret store returned an invalid response."},
	} {
		c, err := New(Connection{URL: "https://delivery.invalid", Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path != BrokerPath || r.Header.Get("X-Vault-Token") != "verified-pod-jwt" {
				t.Error("broker sent another endpoint or identity")
			}
			return &http.Response{StatusCode: test.status, Body: io.NopCloser(strings.NewReader(test.body))}, nil
		})})
		if err != nil {
			t.Fatal(err)
		}
		values, err := c.fetchBroker(t.Context(), []byte("verified-pod-jwt"), []string{"registry/value"})
		var p *publicerr.Error
		if values != nil || !errors.As(err, &p) || p.Reason() != test.reason || strings.Contains(p.Error(), "private") {
			t.Fatal("broker lost refusal or leaked response", p)
		}
		if publicerr.CheckMessage(p.Reason(), 80) != nil || publicerr.CheckMessage(p.Remedy(), 120) != nil {
			t.Fatal("broker diagnostic exceeds its bound")
		}
		c.Close()
	}
}
