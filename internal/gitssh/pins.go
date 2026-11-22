package gitssh

import (
	"bytes"
	"github.com/oberthci/oberth/internal/publicerr"
	"net"
	"os"
	"slices"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// CheckPins reports whether the file pins a usable key for the SSH address.
func CheckPins(path, address string) error {
	data, err := readConfined(path, false)
	if err != nil {
		return err
	}
	callback, err := knownhosts.New(path)
	if err != nil {
		return failure(reasonConfig)
	}
	if len(pinAlgorithms(data, address, callback)) == 0 {
		return failure(reasonPins)
	}
	return nil
}

// pins reads through the owner's custody rule, then parses a private copy so
// the verifier and offered algorithms use exactly the same file bytes.
func pins(path, address, scratch string, read func(string, bool) ([]byte, error)) (ssh.HostKeyCallback, []string, func(), error) {
	data, err := read(path, false)
	if err != nil {
		return nil, nil, nil, err
	}
	f, err := os.CreateTemp(scratch, "oberth-pins-")
	if err != nil {
		return nil, nil, nil, err
	}
	cleanup := func() { _ = f.Close(); _ = os.Remove(f.Name()) }
	if _, err = f.Write(data); err != nil {
		cleanup()
		return nil, nil, nil, err
	}
	if err = f.Close(); err != nil {
		cleanup()
		return nil, nil, nil, err
	}
	callback, err := knownhosts.New(f.Name())
	if err != nil {
		cleanup()
		return nil, nil, nil, failure(reasonConfig)
	}
	algorithms := pinAlgorithms(data, address, callback)
	if len(algorithms) == 0 {
		cleanup()
		return nil, nil, nil, failure(reasonPins)
	}
	return callback, algorithms, cleanup, nil
}
func pinAlgorithms(data []byte, address string, verify ssh.HostKeyCallback) []string {
	var algorithms []string
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil
	}
	remote := &net.TCPAddr{IP: net.ParseIP(host)}
	for len(bytes.TrimSpace(data)) > 0 {
		marker, hosts, key, _, rest, err := ssh.ParseKnownHosts(data)
		if err != nil {
			return nil
		}
		data = rest
		if marker == "cert-authority" {
			if !slices.Contains(hosts, knownhosts.Normalize(address)) {
				continue
			}
			algorithms = append(algorithms, ssh.CertAlgoED25519v01, ssh.CertAlgoECDSA256v01, ssh.CertAlgoECDSA384v01, ssh.CertAlgoECDSA521v01, ssh.CertAlgoRSASHA256v01, ssh.CertAlgoRSASHA512v01)
			continue
		}
		if verify(address, remote, key) != nil {
			continue
		}
		if key.Type() == ssh.KeyAlgoRSA {
			algorithms = append(algorithms, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSASHA512)
		} else {
			algorithms = append(algorithms, key.Type())
		}
	}
	slices.Sort(algorithms)
	return slices.Compact(algorithms)
}
func failure(reason string) error {
	switch reason {
	case reasonPins:
		return publicerr.New(publicerr.Forbidden, ReasonNoPin, "Verify the endpoint and its pinned host key before retrying.", nil)
	case reasonTransport:
		return publicerr.New(publicerr.Unavailable, ReasonTransport, "Check the server address, active identity and pinned host key.", nil)
	default:
		return publicerr.New(publicerr.InvalidInput, "Git SSH configuration is invalid.", "Check the shim configuration, file permissions, identity and pinned host keys.", nil)
	}
}
