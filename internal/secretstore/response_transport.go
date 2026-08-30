package secretstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"syscall"

	vaultapi "github.com/hashicorp/vault/api"
)

// boundedSecretStoreTransport enforces endpoint-specific decoded response
// ceilings before the Vault SDK can parse or copy a body. Error bodies are
// never read: a non-success response retains only its HTTP status, so a store
// cannot reflect request plaintext/ciphertext into durable Oberth errors.
type boundedSecretStoreTransport struct {
	base          http.RoundTripper
	responseLimit func(string) int64
}

func (transport boundedSecretStoreTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := transport.base.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode >= http.StatusMultipleChoices && response.StatusCode < http.StatusBadRequest {
		statusCode := response.StatusCode
		_ = response.Body.Close()
		return nil, &secretStoreResponseStatusError{statusCode: statusCode}
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusBadRequest {
		_ = response.Body.Close()
		response.Body = http.NoBody
		response.ContentLength = 0
		response.Header.Del("Content-Encoding")
		return response, nil
	}

	limitForPath := transport.responseLimit
	if limitForPath == nil {
		limitForPath = secretStoreResponseLimit
	}
	limit := limitForPath(request.URL.Path)
	body, readErr := io.ReadAll(io.LimitReader(response.Body, limit+1))
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil {
		clear(body)
		return nil, &secretStoreResponseReadError{}
	}
	if int64(len(body)) > limit {
		clear(body)
		return nil, &secretStoreResponseLimitError{limit: limit}
	}
	response.Body = &zeroingResponseBody{Reader: bytes.NewReader(body), body: body}
	response.ContentLength = int64(len(body))
	response.Header.Del("Content-Encoding")
	return response, nil
}

func secretStoreResponseLimit(path string) int64 {
	switch {
	case strings.Contains(path, "/encrypt/"):
		return int64(maxTransitCiphertextBytes + maxTransitJSONOverhead)
	case strings.Contains(path, "/decrypt/"):
		return int64(base64EncodedLength(maxTransitPlaintextBytes) + maxTransitJSONOverhead)
	case strings.HasSuffix(path, "/login"):
		return int64(maxAuthJSONResponse)
	case strings.HasSuffix(path, "/auth/token/revoke-self"):
		return int64(maxRevokeResponse)
	default:
		return int64(maxSecretJSONResponse)
	}
}

func base64EncodedLength(size int) int {
	return (size + 2) / 3 * 4
}

type zeroingResponseBody struct {
	*bytes.Reader
	body []byte
}

func (body *zeroingResponseBody) Close() error {
	clear(body.body)
	body.body = nil
	return nil
}

type secretStoreResponseLimitError struct{ limit int64 }

func (err *secretStoreResponseLimitError) Error() string {
	return fmt.Sprintf("secret store response exceeds %d-byte limit", err.limit)
}

type secretStoreResponseReadError struct{}

func (*secretStoreResponseReadError) Error() string { return "secret store response read failed" }

type secretStoreResponseStatusError struct{ statusCode int }

func (err *secretStoreResponseStatusError) Error() string {
	return fmt.Sprintf("secret store returned HTTP %d", err.statusCode)
}

// sealedHint is appended to the two failures a sealed store actually produces.
//
// A sealed OpenBao answers 503 on login, and fails its readiness probe, which
// empties its Service of endpoints so the next connection is refused outright.
// Both were reported as generic transport or status failures, which is the
// same text a misconfigured address or a broken CA produces -- so the one
// cause with a one-command fix looked like the causes that need an
// investigation.
const sealedHint = " (the store may be sealed; run: oberth unseal)"

// sealedStatus is the status a sealed store returns to a login attempt.
const sealedStatus = http.StatusServiceUnavailable

func sanitizeSecretStoreHTTPError(operation string, err error) error {
	var statusErr *secretStoreResponseStatusError
	if errors.As(err, &statusErr) {
		return fmt.Errorf("secret store %s failed: HTTP %d %s%s", operation, statusErr.statusCode,
			http.StatusText(statusErr.statusCode), hintForStatus(statusErr.statusCode))
	}
	var responseErr *vaultapi.ResponseError
	if errors.As(err, &responseErr) {
		return fmt.Errorf("secret store %s failed: HTTP %d %s%s", operation, responseErr.StatusCode,
			http.StatusText(responseErr.StatusCode), hintForStatus(responseErr.StatusCode))
	}
	var limitErr *secretStoreResponseLimitError
	if errors.As(err, &limitErr) {
		return fmt.Errorf("secret store %s failed: response exceeds %d-byte limit", operation, limitErr.limit)
	}
	var readErr *secretStoreResponseReadError
	if errors.As(err, &readErr) {
		return errors.New("secret store " + operation + " failed: response read error")
	}
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("secret store %s failed: %w", operation, context.Canceled)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("secret store %s failed: %w", operation, context.DeadlineExceeded)
	}
	// A refused connection carries no store-supplied content -- it is this
	// process's own dial result -- so naming it leaks nothing and is the
	// difference between "transport error" and a fix.
	if errors.Is(err, syscall.ECONNREFUSED) {
		return errors.New("secret store " + operation + " failed: connection refused" + sealedHint)
	}
	return errors.New("secret store " + operation + " failed: transport error")
}

// hintForStatus names the sealed store behind the one status it produces.
func hintForStatus(status int) string {
	if status == sealedStatus {
		return sealedHint
	}
	return ""
}

func secretStoreRetryPolicy(ctx context.Context, response *http.Response, err error) (bool, error) {
	var statusErr *secretStoreResponseStatusError
	var limitErr *secretStoreResponseLimitError
	var readErr *secretStoreResponseReadError
	if errors.As(err, &statusErr) || errors.As(err, &limitErr) || errors.As(err, &readErr) {
		return false, nil
	}
	return vaultapi.DefaultRetryPolicy(ctx, response, err)
}
