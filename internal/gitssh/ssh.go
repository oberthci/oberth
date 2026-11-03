package gitssh

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"slices"
	"time"

	hostkeys "github.com/skeema/knownhosts"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// ExitCode preserves the remote Git service's exit status without forwarding
// untrusted exception text to the local transport diagnostic.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	if errors.Is(err, ErrRefused) {
		return 126
	}
	var remote *ssh.ExitError
	if errors.As(err, &remote) && remote.ExitStatus() > 0 && remote.ExitStatus() <= 255 {
		return remote.ExitStatus()
	}
	return 255
}

// Run serves one transport process. The caller exits after its remote result;
// a non-pollable stdin read must not delay that result or keep stdout open.
func Run(ctx context.Context, arguments []string, configPath string, input io.ReadCloser, output, diagnostics io.Writer) error {
	value, err := parse(arguments)
	if err != nil {
		return refusal("SSH arguments refused")
	}
	value, err = configured(configPath, value)
	if err != nil {
		return refusal("SSH configuration refused")
	}
	secret, err := readFile(value.identity, true)
	if err != nil {
		return refusal("SSH identity unavailable or permissions refused")
	}
	signer, err := ssh.ParsePrivateKey(secret)
	clear(secret)
	if err != nil {
		return refusal("SSH identity parse refused")
	}
	pins, err := readFile(value.knownHosts, false)
	if err != nil {
		return refusal("pinned known_hosts unavailable")
	}
	file, err := os.CreateTemp("", "oberth-git-ssh-hosts-")
	if err != nil {
		return refusal("pinned host-key snapshot unavailable")
	}
	path := file.Name()
	defer func() { _ = os.Remove(path) }()
	_, writeErr := file.Write(pins)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		return refusal("pinned host-key snapshot failed")
	}
	callback, err := knownhosts.New(path)
	if err != nil {
		return refusal("pinned host keys invalid")
	}
	database, err := hostkeys.NewDB(path)
	if err != nil {
		return refusal("pinned host keys invalid")
	}
	address := net.JoinHostPort(value.host, value.port)
	algorithms := database.HostKeyAlgorithms(address)
	for _, pin := range database.HostKeys(address) {
		if pin.Cert {
			algorithms = append(algorithms, ssh.CertAlgoED25519v01, ssh.CertAlgoECDSA256v01, ssh.CertAlgoECDSA384v01, ssh.CertAlgoECDSA521v01, ssh.CertAlgoRSASHA256v01, ssh.CertAlgoRSASHA512v01)
			break
		}
	}
	algorithms = slices.DeleteFunc(algorithms, func(a string) bool { return a == ssh.KeyAlgoRSA || a == ssh.CertAlgoRSAv01 })
	if len(algorithms) == 0 {
		return refusal("SSH endpoint has no pinned host key")
	}
	connection, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", address)
	if err != nil {
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		return transportFailure("SSH connection failed")
	}
	defer func() { _ = connection.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = connection.Close(); _ = input.Close() })
	defer stop()
	deadline := time.Now().Add(10 * time.Second)
	if end, ok := ctx.Deadline(); ok && end.Before(deadline) {
		deadline = end
	}
	if err := connection.SetDeadline(deadline); err != nil {
		return sessionFailure(err, "SSH connection deadline failed")
	}
	hostKeyRefused := false
	verifyHostKey := func(host string, remote net.Addr, key ssh.PublicKey) error {
		err := callback(host, remote, key)
		hostKeyRefused = err != nil
		return err
	}
	clientConnection, channels, requests, err := ssh.NewClientConn(connection, address, &ssh.ClientConfig{
		User: value.user, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, HostKeyCallback: verifyHostKey, HostKeyAlgorithms: algorithms,
	})
	if err != nil {
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		if hostKeyRefused {
			return refusal("SSH pinned host key refused")
		}
		return sessionFailure(err, "SSH handshake failed")
	}
	if err := connection.SetDeadline(time.Time{}); err != nil {
		_ = clientConnection.Close()
		return sessionFailure(err, "SSH connection deadline failed")
	}
	client := ssh.NewClient(clientConnection, channels, requests)
	defer func() { _ = client.Close() }()
	session, err := client.NewSession()
	if err != nil {
		return sessionFailure(err, "SSH session refused")
	}
	defer func() { _ = session.Close() }()
	session.Stdout, session.Stderr = output, diagnostics
	if protocol := os.Getenv("GIT_PROTOCOL"); protocol != "" {
		if protocol != "version=1" && protocol != "version=2" {
			return refusal("unsupported Git wire protocol")
		}
		_ = session.Setenv("GIT_PROTOCOL", protocol) // Servers may decline and use protocol v0.
	}
	pipe, err := session.StdinPipe()
	if err != nil {
		return sessionFailure(err, "SSH stdin unavailable")
	}
	if err := session.Start(value.command); err != nil {
		return sessionFailure(err, "SSH Git service refused")
	}
	go func() { _, _ = io.Copy(pipe, input); _ = pipe.Close() }()
	err = session.Wait()
	_ = input.Close()
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	if err != nil {
		var remote *ssh.ExitError
		if !errors.As(err, &remote) {
			var missing *ssh.ExitMissingError
			if errors.As(err, &missing) {
				return transportFailure("SSH service disconnected without status")
			}
			return sessionFailure(err, "SSH service stream failed")
		}
	}
	return err
}
