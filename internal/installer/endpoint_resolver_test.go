package installer

import (
	"crypto/tls"
	"errors"
	"net/http"
	"reflect"
	"testing"

	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/publicerr"
)

func TestInstallationUsesTheClientHostResolver(t *testing.T) {
	transport := newEndpointTransport(&tls.Config{MinVersion: tls.VersionTLS12}, nil)
	defer transport.close()
	if reflect.ValueOf(transport.connect).Pointer() != reflect.ValueOf(client.DialContext).Pointer() {
		t.Fatal("installation bypasses the client host resolver")
	}
}

type refusedResolution struct{ cause error }

func (r refusedResolution) RoundTrip(*http.Request) (*http.Response, error) { return nil, r.cause }

func TestEndpointRetainsActionableResolutionRefusal(t *testing.T) {
	cause := publicerr.New(publicerr.Unavailable, "Name unavailable.", "Use a resolvable server address.", nil)
	remote := &http.Client{Transport: refusedResolution{cause}}
	_, err := evaluateEndpoint(t.Context(), remote, "https://oberth.local/readyz")
	var problem *publicerr.Error
	var retryable *Problem
	if !errors.As(err, &problem) || problem != cause || errors.As(err, &retryable) {
		t.Fatal("installation turned a resolution refusal into a rollout retry", err)
	}
}
