package gitssh

import (
	"errors"
	"fmt"
	"io"
	"net"

	"golang.org/x/crypto/ssh"
)

// ErrTransportUnavailable identifies a temporary network or stream failure.
var ErrTransportUnavailable = errors.New("temporary SSH transport failure")

// ErrRefused identifies a host-key, identity, configuration or service refusal.
var ErrRefused = errors.New("SSH policy refused")

const TemporaryFailureMessage = "oberth git-ssh: temporary transport failure"
const RefusalMessage = "oberth git-ssh: host key, identity or configuration refused"
const serviceFailureMessage = "oberth git-ssh: upstream Git service failed"

// Diagnostic returns only a fixed classification, never upstream or key text.
func Diagnostic(err error) string {
	if errors.Is(err, ErrRefused) {
		return RefusalMessage
	}
	if errors.Is(err, ErrTransportUnavailable) {
		return TemporaryFailureMessage
	}
	return serviceFailureMessage
}

func refusal(message string) error { return fmt.Errorf("%w: %s", ErrRefused, message) }

func transportFailure(message string) error {
	return fmt.Errorf("%w: %s", ErrTransportUnavailable, message)
}

func temporaryIO(err error) bool {
	var network net.Error
	return errors.As(err, &network) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed)
}

func sessionFailure(err error, message string) error {
	var channel *ssh.OpenChannelError
	if temporaryIO(err) || (errors.As(err, &channel) && (channel.Reason == ssh.ResourceShortage || channel.Reason == ssh.ConnectionFailed)) {
		return transportFailure(message)
	}
	return refusal(message)
}
