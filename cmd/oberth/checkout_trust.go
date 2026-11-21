package main

import (
	"github.com/oberthci/oberth/internal/gitssh"
	"github.com/oberthci/oberth/internal/publicerr"
	"net"
	"net/url"
	"path/filepath"
)

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
		problem := publicerr.New(publicerr.InvalidInput, "SSH trust file has no usable endpoint key.", "Pass the SSH trust file printed by install.", err)
		problem.ID = "ssh-trust-endpoint"
		problem.Context = []string{"File: " + absolute}
		return problem
	}
	return nil
}
