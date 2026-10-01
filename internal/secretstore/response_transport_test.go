package secretstore

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

type trackedResponseBody struct {
	io.Reader
	closed bool
}

func (body *trackedResponseBody) Close() error {
	body.closed = true
	return nil
}

func TestBoundedSecretStoreTransportRejectsSuccessfulOversizedResponsesBeforeVaultParsing(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		path string
	}{
		{name: "login", path: "/v1/auth/kubernetes/login"},
		{name: "transit encrypt", path: "/v1/oberth-transit/encrypt/trusted-plan-artifacts"},
		{name: "transit decrypt", path: "/v1/oberth-transit/decrypt/trusted-plan-artifacts"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			body := &trackedResponseBody{Reader: io.LimitReader(strings.NewReader(strings.Repeat("x", 32)), 9)}
			transport := boundedSecretStoreTransport{
				base: roundTripFunc(func(*http.Request) (*http.Response, error) {
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     make(http.Header),
						Body:       body,
					}, nil
				}),
				responseLimit: func(string) int64 { return 8 },
			}
			request, err := http.NewRequestWithContext(context.Background(), http.MethodPut, "https://openbao.invalid"+test.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			response, err := transport.RoundTrip(request)
			if response != nil {
				t.Fatalf("RoundTrip() response = %#v, want nil", response)
			}
			var limitErr *secretStoreResponseLimitError
			if !errors.As(err, &limitErr) || limitErr.limit != 8 {
				t.Fatalf("RoundTrip() error = %v, want 8-byte limit error", err)
			}
			if !body.closed {
				t.Fatal("RoundTrip() did not close oversized response body")
			}
		})
	}
}
