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
	var remote *ssh.ExitError
	if errors.As(err, &remote) && remote.ExitStatus() > 0 && remote.ExitStatus() <= 255 {
		return remote.ExitStatus()
	}
	return 255
}

// Run owns input for the life of this one SSH transport. Closing it on exit
// joins the stdin copier even when the Git parent stops supplying input.
func Run(ctx context.Context, arguments []string, configPath string, input io.ReadCloser, output, diagnostics io.Writer) error {
	value, err := parse(arguments)
	if err != nil {
		return err
	}
	value, err = configured(configPath, value)
	if err != nil {
		return err
	}
	secret, err := readFile(value.identity, true)
	if err != nil {
		return errors.New("SSH identity unavailable or permissions refused")
	}
	signer, err := ssh.ParsePrivateKey(secret)
	clear(secret)
	if err != nil {
		return errors.New("SSH identity parse refused")
	}
	pins, err := readFile(value.knownHosts, false)
	if err != nil {
		return errors.New("pinned known_hosts unavailable")
	}
	file, err := os.CreateTemp("", "oberth-git-ssh-hosts-")
	if err != nil {
		return errors.New("pinned host-key snapshot unavailable")
	}
	path := file.Name()
	defer func() { _ = os.Remove(path) }()
	_, writeErr := file.Write(pins)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		return errors.New("pinned host-key snapshot failed")
	}
	callback, err := knownhosts.New(path)
	if err != nil {
		return errors.New("pinned host keys invalid")
	}
	database, err := hostkeys.NewDB(path)
	if err != nil {
		return errors.New("pinned host keys invalid")
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
		return errors.New("SSH endpoint has no pinned host key")
	}
	connection, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", address)
	if err != nil {
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		return errors.New("SSH connection failed")
	}
	defer func() { _ = connection.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = connection.Close(); _ = input.Close() })
	defer stop()
	deadline := time.Now().Add(10 * time.Second)
	if end, ok := ctx.Deadline(); ok && end.Before(deadline) {
		deadline = end
	}
	if err := connection.SetDeadline(deadline); err != nil {
		return err
	}
	clientConnection, channels, requests, err := ssh.NewClientConn(connection, address, &ssh.ClientConfig{
		User: value.user, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, HostKeyCallback: callback, HostKeyAlgorithms: algorithms,
	})
	if err != nil {
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		return errors.New("SSH handshake or pinned host key refused")
	}
	if err := connection.SetDeadline(time.Time{}); err != nil {
		_ = clientConnection.Close()
		return err
	}
	client := ssh.NewClient(clientConnection, channels, requests)
	defer func() { _ = client.Close() }()
	session, err := client.NewSession()
	if err != nil {
		return errors.New("SSH session refused")
	}
	defer func() { _ = session.Close() }()
	session.Stdout, session.Stderr = output, diagnostics
	if protocol := os.Getenv("GIT_PROTOCOL"); protocol != "" {
		if protocol != "version=1" && protocol != "version=2" {
			return errors.New("unsupported Git wire protocol")
		}
		_ = session.Setenv("GIT_PROTOCOL", protocol) // Servers may decline and use protocol v0.
	}
	pipe, err := session.StdinPipe()
	if err != nil {
		return errors.New("SSH stdin unavailable")
	}
	if err := session.Start(value.command); err != nil {
		return errors.New("SSH Git service refused")
	}
	copied := make(chan struct{})
	go func() { _, _ = io.Copy(pipe, input); _ = pipe.Close(); close(copied) }()
	err = session.Wait()
	_ = input.Close()
	<-copied
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	return err
}
