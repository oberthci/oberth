package secretstore

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
	"unicode"

	vaultapi "github.com/hashicorp/vault/api"
)

// boundedSecretStoreTransport enforces endpoint-specific decoded response
// ceilings before the Vault SDK can parse or copy a body. Only PKI issuance
// errors may include bounded diagnostics. All other error bodies are discarded
// so request plaintext/ciphertext cannot be reflected into durable errors.
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
		if isPKIIssuePath(request.URL.Path) {
			body, readErr := io.ReadAll(io.LimitReader(response.Body, 1025))
			_ = response.Body.Close()
			defer clear(body)
			var payload struct {
				Errors []string `json:"errors"`
			}
			detail := ""
			if readErr == nil && len(body) <= 1024 && json.Unmarshal(body, &payload) == nil {
				detail = strings.Map(func(r rune) rune {
					if unicode.IsControl(r) {
						return ' '
					}
					return r
				}, strings.Join(payload.Errors, "; "))
			}
			return nil, &StatusError{Code: response.StatusCode, Detail: detail}
		}
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

func isPKIIssuePath(path string) bool {
	parts := strings.Split(strings.TrimPrefix(path, "/v1/"), "/")
	return len(parts) == 3 && pkiSegment.MatchString(parts[0]) && parts[1] == "issue" && pkiSegment.MatchString(parts[2])
}

// StatusError is a sanitized HTTP failure. Detail is populated only for a
// bounded PKI issuance errors[] response, never login, KV or Transit responses.
type StatusError struct {
	Op     string
	Code   int
	Detail string
}

func (err *StatusError) Error() string {
	message := fmt.Sprintf("secret store %s failed: HTTP %d %s", err.Op, err.Code, http.StatusText(err.Code))
	if err.Detail != "" {
		message += ": " + err.Detail
	}
	return message
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

func sanitizeSecretStoreHTTPError(operation string, err error) error {
	var typedStatus *StatusError
	if errors.As(err, &typedStatus) {
		return &StatusError{Op: operation, Code: typedStatus.Code, Detail: typedStatus.Detail}
	}
	var statusErr *secretStoreResponseStatusError
	if errors.As(err, &statusErr) {
		return &StatusError{Op: operation, Code: statusErr.statusCode}
	}
	var responseErr *vaultapi.ResponseError
	if errors.As(err, &responseErr) {
		return &StatusError{Op: operation, Code: responseErr.StatusCode}
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
	var unknownCA x509.UnknownAuthorityError
	var networkErr net.Error
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return errors.New("secret store " + operation + " failed: connection refused")
	case errors.As(err, &unknownCA):
		return errors.New("secret store " + operation + " failed: unknown CA")
	case errors.As(err, &networkErr) && networkErr.Timeout():
		return errors.New("secret store " + operation + " failed: timeout")
	}
	return errors.New("secret store " + operation + " failed: transport error")
}

func secretStoreRetryPolicy(ctx context.Context, response *http.Response, err error) (bool, error) {
	var typedStatus *StatusError
	var statusErr *secretStoreResponseStatusError
	var limitErr *secretStoreResponseLimitError
	var readErr *secretStoreResponseReadError
	if errors.As(err, &typedStatus) || errors.As(err, &statusErr) || errors.As(err, &limitErr) || errors.As(err, &readErr) {
		return false, nil
	}
	return vaultapi.DefaultRetryPolicy(ctx, response, err)
}
