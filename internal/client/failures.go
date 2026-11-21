package client

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/restartretry"
)

func retryRequest(err error) bool {
	if failedInitialDial(err) {
		return false
	}
	return restartretry.Transient(err)
}

func failedInitialDial(err error) bool {
	var operation *net.OpError
	return errors.As(err, &operation) && operation.Op == "dial"
}

func requestFailure(d apicontract.Definition, err error) error {
	var p *publicerr.Error
	if errors.As(err, &p) {
		return err
	}
	key := failureClass(err)
	if d.Mutation && !failedInitialDial(err) && (key == "connect" || key == "deadline" || key == "cancelled") {
		return uncertain(d, err)
	}
	return failure(key, err)
}

func uncertain(d apicontract.Definition, cause error) error {
	remedy := "Read the operation's current state before submitting it again."
	if check, ok := toolDefinition(d.Check); ok && check.Command != "" && check.Command != "-" {
		remedy = fmt.Sprintf("Check with oberth %s before submitting it again.", check.Command)
	}
	reason := "The operation's outcome is unknown."
	if errors.Is(cause, context.Canceled) {
		reason = "Request interrupted; the operation's outcome is unknown."
	}
	if errors.Is(cause, context.DeadlineExceeded) {
		reason = "Request timed out; the operation's outcome is unknown."
	}
	return &unknownOutcome{publicerr.New(publicerr.Unavailable, reason, remedy, cause)}
}
func replyFailure(d apicontract.Definition, err error) error {
	if d.Mutation {
		return uncertain(d, err)
	}
	if errors.Is(err, apicontract.ErrReplySize) {
		return failure("size", err)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return requestFailure(d, err)
	}
	return failure("reply", err)
}

func failureClass(err error) string {
	key := "connect"
	var authority x509.UnknownAuthorityError
	var invalid x509.CertificateInvalidError
	var hostname x509.HostnameError
	var network net.Error
	switch {
	case errors.Is(err, context.Canceled):
		key = "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		key = "deadline"
	case refusedClientCertificate(err):
		key = "client_cert"
	case errors.As(err, &authority):
		key = "authority"
	case errors.As(err, &hostname):
		key = "hostname"
	case errors.As(err, &invalid) && invalid.Reason == x509.Expired:
		key = "expired"
	case errors.As(err, &network) && network.Timeout():
		key = "deadline"
	}
	return key
}

func refusedClientCertificate(err error) bool {
	var operation *net.OpError
	if !errors.As(err, &operation) || operation.Op != "remote error" {
		return false
	}
	switch operation.Err.Error() {
	case "tls: bad certificate", "tls: expired certificate", "tls: certificate expired", "tls: unknown certificate authority", "tls: certificate required", "tls: access denied", "tls: unknown certificate":
		return true
	}
	return false
}

// A response proves arrival even when following its redirect is refused.
func exchangeFailure(d apicontract.Definition, response *http.Response, err error) (bool, error) {
	if response != nil && d.Mutation {
		return false, replyFailure(d, err)
	}
	return retryRequest(err), requestFailure(d, err)
}
