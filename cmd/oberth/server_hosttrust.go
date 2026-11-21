package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/reponame"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

type hostScanner struct{}

func (hostScanner) Scan(ctx context.Context, address, algorithm string) (ssh.PublicKey, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		if err = conn.SetDeadline(deadline); err != nil {
			return nil, err
		}
	}
	var offered ssh.PublicKey
	cfg := &ssh.ClientConfig{User: "git", HostKeyAlgorithms: []string{algorithm}, HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
		offered = key
		return errors.New("host key received")
	}}
	_, _, _, err = ssh.NewClientConn(conn, address, cfg)
	if offered != nil {
		return offered, nil
	}
	return nil, err
}

const forgeTrustID = "forge-trust-unconfirmed"

const forgeTrustReason = "Forge host trust was not confirmed."

func (a serverAdministration) pinsPath() string {
	return filepath.Join(filepath.Dir(a.owner.c.config.DatabaseFile), "host-pins", "known_hosts")
}
func (a serverAdministration) preparePins(ctx context.Context, endpoint *url.URL, fingerprint string, repin bool) (app.TrustResult, func(context.Context) error, error) {
	port := endpoint.Port()
	if port == "" {
		port = "22"
	}
	address := net.JoinHostPort(endpoint.Hostname(), port)
	request := app.TrustRequest{Address: address, Fingerprints: []string{fingerprint}}
	old, err := os.ReadFile(a.pinsPath())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return app.TrustResult{}, nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var result app.TrustResult
	if repin {
		result, err = app.Repin(ctx, hostScanner{}, request, old)
	} else {
		result, err = app.TrustHost(ctx, hostScanner{}, request)
		if err == nil {
			result.Lines, err = a.addPins(address, old, result.Lines)
		}
	}
	if err != nil {
		if len(result.Offered) == 0 {
			problem := publicerr.New(publicerr.Unavailable, "No SSH host key was received from the forge.", "Check the forge address and SSH availability, then retry.", err)
			problem.Context = []string{"Forge endpoint: " + address}
			var scan *app.Refusal
			if errors.As(err, &scan) {
				problem.Detail = scan.Detail
			}
			return result, nil, problem
		}
		problem := publicerr.New(publicerr.Forbidden, forgeTrustReason, "Verify an offered fingerprint, then retry with --expected-host-fingerprint; use --repin only for a changed key.", err)
		problem.ID = forgeTrustID
		problem.Context = []string{"Offered fingerprints: " + strings.Join(result.Offered, ", ")}
		return result, nil, problem
	}
	return result, func(ctx context.Context) error { return a.savePins(ctx, old, result.Lines) }, nil
}
func (a serverAdministration) addPins(address string, old, trusted []byte) ([]byte, error) {
	if len(old) > 0 {
		verify, err := knownhosts.New(a.pinsPath())
		if err != nil {
			return nil, err
		}
		_, _, key, _, _, err := ssh.ParseKnownHosts(trusted)
		if err != nil {
			return nil, err
		}
		err = verify(address, forgeAddress(address), key)
		if err == nil {
			return old, nil
		}
		var mismatch *knownhosts.KeyError
		if !errors.As(err, &mismatch) || len(mismatch.Want) > 0 {
			return nil, err
		}
	}
	if len(old) > 0 && old[len(old)-1] != '\n' {
		old = append(bytes.Clone(old), '\n')
	}
	return append(bytes.Clone(old), trusted...), nil
}
func (a serverAdministration) savePins(ctx context.Context, before, pins []byte) error {
	a.owner.pinsMu.Lock()
	defer a.owner.pinsMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	current, err := os.ReadFile(a.pinsPath())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if !bytes.Equal(current, before) {
		return publicerr.New(publicerr.Conflict, "Forge host pins changed during confirmation.", "Retry the upstream command against the current pins.", nil)
	}
	if err = os.MkdirAll(filepath.Dir(a.pinsPath()), 0700); err != nil {
		return err
	}
	return writeServerState(a.pinsPath(), pins)
}

func upstreamOrganization(q apicontract.UpstreamAddArgs, endpoint *url.URL) (string, error) {
	namespace := strings.Trim(endpoint.Path, "/")
	check := q.CheckRepo
	if check == "" {
		if namespace == "" {
			return "", publicerr.New(publicerr.InvalidInput, "The upstream organization is missing.", "Supply a repository to probe when adding the upstream.", nil)
		}
		check = path.Base(namespace) + "/repository"
	}
	repo, err := reponame.ParseRepository(q.Name + "/" + check)
	if err != nil {
		return "", err
	}
	if namespace != "" && path.Base(namespace) != repo.Organization {
		return "", probeScopeError(nil)
	}
	return repo.Organization, nil
}

func probeScopeError(cause error) error {
	return publicerr.New(publicerr.InvalidInput, "The probe repository is outside the upstream.", "Use an organization/repository name within this upstream.", cause)
}
