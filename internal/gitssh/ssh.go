package gitssh

import (
	"context"
	"errors"
	"github.com/oberthci/oberth/internal/publicerr"
	"golang.org/x/crypto/ssh/knownhosts"
	"io"
	"net"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// HostKeyMismatchReason is the public refusal preserved across the Git child boundary.
const HostKeyMismatchReason = "Git SSH host key does not match the pin."

type IO struct {
	Input               io.ReadCloser
	Output, Diagnostics io.Writer
}

// Run connects with one selected private key and endpoint pins. Owned input
// copying is closed and joined before return, including on cancellation.
func Run(ctx context.Context, c Config, streams IO) error {
	input := streams.Input
	if c.Now == nil || input == nil {
		return failure(reasonConfig)
	}
	input, err := pollableInput(input)
	if err != nil {
		return err
	}
	defer func() { _ = input.Close() }()
	streams.Input = input
	private, err := c.readFile(c.Identity, true)
	if err != nil {
		return err
	}
	defer clear(private)
	signer, err := ssh.ParsePrivateKey(private)
	if err != nil {
		return failure(reasonConfig)
	}
	verify, algorithms, cleanup, err := pins(c.KnownHosts, c.address(), c.Scratch, c.readFile)
	if err != nil {
		return err
	}
	defer cleanup()
	dial := c.Dial
	if dial == nil {
		dial = func(ctx context.Context, address string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", address)
		}
	}
	conn, err := dial(ctx, c.address())
	if err != nil {
		return interrupted(ctx, c.Command, unanswered)
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close(); _ = input.Close() })
	defer stop()
	if err = conn.SetDeadline(c.Now().Add(10 * time.Second)); err != nil {
		return transportError(ctx)
	}
	native, channels, requests, err := ssh.NewClientConn(conn, c.address(), &ssh.ClientConfig{User: c.User, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, HostKeyCallback: verify, HostKeyAlgorithms: algorithms})
	if err != nil {
		return handshakeError(ctx, c.Command, err)
	}
	client := ssh.NewClient(native, channels, requests)
	defer func() { _ = client.Close() }()
	if err = conn.SetDeadline(time.Time{}); err != nil {
		return transportError(ctx)
	}
	return stream(ctx, client, c.Command, streams)
}
func stream(ctx context.Context, client *ssh.Client, command string, streams IO) error {
	session, err := client.NewSession()
	if err != nil {
		return interrupted(ctx, command, lost)
	}
	defer func() { _ = session.Close() }()
	input := streams.Input
	session.Stdout = streams.Output
	session.Stderr = streams.Diagnostics
	pipe, err := session.StdinPipe()
	if err != nil {
		return transportError(ctx)
	}
	if err = session.Start(command); err != nil {
		return interrupted(ctx, command, lost)
	}
	done := make(chan struct{})
	go func() { _, _ = io.Copy(pipe, input); _ = pipe.Close(); close(done) }()
	err = session.Wait()
	_ = input.Close()
	<-done
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	var remote *ssh.ExitError
	if err != nil && !errors.As(err, &remote) {
		return interrupted(ctx, command, lost)
	}
	return err
}
func transportError(ctx context.Context) error {
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	return failure(reasonTransport)
}

// A host key that differs from the pin, is revoked in the pin file, or is of a
// type the pin does not offer is a host-key failure: retrying cannot succeed.
func handshakeError(ctx context.Context, command string, err error) error {
	var pins *knownhosts.KeyError
	var revoked *knownhosts.RevokedError
	if errors.As(err, &pins) || errors.As(err, &revoked) || strings.Contains(err.Error(), "no common algorithm for host key") {
		return publicerr.New(publicerr.Forbidden, HostKeyMismatchReason, "Verify the server fingerprint and correct the pinned host key.", err)
	}
	if strings.Contains(err.Error(), "unable to authenticate") || strings.Contains(err.Error(), "too many authentication failures") {
		return publicerr.New(publicerr.Forbidden, "Git SSH identity was refused.", "Use an active identity; ask the administrator to check revocation and access.", err)
	}
	return interrupted(ctx, command, ended)
}

// The pins were read for this exact address, so a dial that fails reached no
// server rather than a wrong one. A handshake that ends without a key or
// identity refusal cannot be told from a server going away; its remedy says
// both. A loss after the session began is the server going away.
const (
	unanswered = iota
	ended
	lost
)

func interrupted(ctx context.Context, command string, stage int) error {
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	verb := "Fetch"
	if strings.HasPrefix(command, "git-receive-pack") {
		verb = "Push"
	}
	again := verb + " again in a moment; the server may be restarting."
	switch stage {
	case unanswered:
		return publicerr.New(publicerr.Unavailable, ReasonUnanswered, again, nil)
	case ended:
		return publicerr.New(publicerr.Unavailable, "The Git server ended the connection before Git began.", verb+" again in a moment; if it repeats, ask the administrator to check this identity.", nil)
	}
	return publicerr.New(publicerr.Unavailable, ReasonLost, again, nil)
}
