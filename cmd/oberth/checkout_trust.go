package main

import (
	"github.com/oberthci/oberth/internal/gitssh"
	"github.com/oberthci/oberth/internal/publicerr"
	"net"
	"net/url"
	"path/filepath"
)

// serverTrustError identifies a missing or mismatched server pin independently
// of the remedy's wording, so join can ask for a new invitation.
type serverTrustError struct{ problem *publicerr.Error }

func (e *serverTrustError) Error() string { return e.problem.Error() }
func (e *serverTrustError) Unwrap() error { return e.problem }

func sshPort(endpoint *url.URL) string {
	if port := endpoint.Port(); port != "" {
		return port
	}
	return "22"
}

func checkCheckoutTrust(known string, endpoint *url.URL) error {
	if err := checkHostFile(known); err != nil {
		return err
	}
	absolute, err := filepath.Abs(known)
	if err != nil {
		return err
	}
	if err = gitssh.CheckPins(absolute, net.JoinHostPort(endpoint.Hostname(), sshPort(endpoint))); err != nil {
		problem := publicerr.New(publicerr.InvalidInput, "The file has no SSH host key of this Oberth server.", serverTrustRemedy, err)
		problem.ID = "ssh-trust-endpoint"
		problem.Context = []string{"File: " + absolute}
		return &serverTrustError{problem}
	}
	return nil
}
