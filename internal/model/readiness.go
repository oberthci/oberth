package model

import "errors"

// ReadinessResponse contains only fixed public codes, never dependency errors.
type ReadinessResponse struct {
	Status string `json:"status"`
	Code   string `json:"code"`
}

type readinessFailure struct {
	code string
	err  error
}

func (failure readinessFailure) Error() string { return failure.err.Error() }
func (failure readinessFailure) Unwrap() error { return failure.err }

// ReadinessFailure preserves an internal cause while giving public diagnostics
// a bounded vocabulary. Unknown codes are replaced with unavailable.
func ReadinessFailure(code string, err error) error {
	if err == nil {
		return nil
	}
	return readinessFailure{code: publicReadinessCode(code), err: err}
}

func publicReadinessCode(code string) string {
	switch code {
	case "stale", "canceled", "deadline", "dependencies", "database_error", "no_upstream", "ssh_identity", "startup_recovery", "draining":
		return code
	default:
		return "unavailable"
	}
}

// Readiness reports the same safe payload over HTTP and the local admin socket.
func Readiness(err error) ReadinessResponse {
	if err == nil {
		return ReadinessResponse{Status: "ready", Code: "ready"}
	}
	code := "unavailable"
	var failure readinessFailure
	if errors.As(err, &failure) {
		code = publicReadinessCode(failure.code)
	}
	return ReadinessResponse{Status: "not ready", Code: code}
}

// Valid rejects arbitrary strings from remote diagnostic responses.
func (response ReadinessResponse) Valid() bool {
	return response.Status == "ready" && response.Code == "ready" ||
		response.Status == "not ready" && response.Code == publicReadinessCode(response.Code)
}
