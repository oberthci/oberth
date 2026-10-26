package gitcache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"

	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"golang.org/x/crypto/ssh"
)

type strictSSHSession struct {
	ctx        context.Context
	connection net.Conn
	client     *ssh.Client
	session    *ssh.Session
	stdin      io.WriteCloser
	stdout     io.Reader
	stop       func() bool
	once       sync.Once
}

func newStrictSSHSession(ep *transport.Endpoint, auth transport.AuthMethod, service string) (result *strictSSHSession, err error) {
	credentials, ok := auth.(*strictSSHAuth)
	if !ok || credentials.config.HostKeyCallback == nil {
		return nil, errors.New("explicit pinned SSH authentication is required")
	}
	ctx := credentials.ctx
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", sshAddress(ep))
	if err != nil {
		return nil, err
	}
	session := &strictSSHSession{ctx: ctx, connection: conn}
	session.stop = context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer func() {
		if err != nil {
			_ = session.Close()
		}
	}()
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return nil, err
		}
	}
	clientConn, channels, requests, err := ssh.NewClientConn(conn, sshAddress(ep), credentials.config)
	if err != nil {
		return nil, err
	}
	session.client = ssh.NewClient(clientConn, channels, requests)
	session.session, err = session.client.NewSession()
	if err != nil {
		return nil, err
	}
	session.stdin, err = session.session.StdinPipe()
	if err != nil {
		return nil, err
	}
	session.stdout, err = session.session.StdoutPipe()
	if err != nil {
		return nil, err
	}
	session.session.Stderr = io.Discard
	path := "'" + strings.ReplaceAll(ep.Path, "'", "'\\''") + "'"
	if err = session.session.Start(service + " " + path); err != nil {
		return nil, err
	}
	return session, nil
}
func (s *strictSSHSession) Close() error {
	s.once.Do(func() {
		s.stop()
		if s.session != nil {
			_ = s.session.Close()
		}
		if s.client != nil {
			_ = s.client.Close()
		}
		_ = s.connection.Close()
	})
	return nil
}
func (s *strictSSHSession) AdvertisedReferences() (*packp.AdvRefs, error) {
	return s.AdvertisedReferencesContext(s.ctx)
}
func (s *strictSSHSession) AdvertisedReferencesContext(ctx context.Context) (*packp.AdvRefs, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	refs := packp.NewAdvRefs()
	if err := refs.Decode(s.stdout); err != nil {
		return nil, err
	}
	return refs, nil
}
func (s *strictSSHSession) UploadPack(ctx context.Context, req *packp.UploadPackRequest) (*packp.UploadPackResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := req.UploadRequest.Encode(s.stdin); err != nil {
		return nil, err
	}
	if err := req.UploadHaves.Encode(s.stdin, true); err != nil {
		return nil, err
	}
	if err := pktline.NewEncoder(s.stdin).EncodeString("done\n"); err != nil {
		return nil, err
	}
	if err := s.stdin.Close(); err != nil {
		return nil, err
	}
	response := packp.NewUploadPackResponse(req)
	if err := response.Decode(io.NopCloser(s.stdout)); err != nil {
		return nil, err
	}
	return response, nil
}
func (s *strictSSHSession) ReceivePack(ctx context.Context, req *packp.ReferenceUpdateRequest) (*packp.ReportStatus, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := req.Encode(s.stdin); err != nil {
		return nil, err
	}
	if err := s.stdin.Close(); err != nil {
		return nil, err
	}
	if !req.Capabilities.Supports(capability.ReportStatus) {
		return nil, errors.New("upstream must support report-status for attributable publication")
	}
	response := packp.NewReportStatus()
	if err := response.Decode(s.stdout); err != nil {
		return nil, err
	}
	if err := response.Error(); err != nil {
		return response, err
	}
	if err := s.session.Wait(); err != nil {
		return response, fmt.Errorf("upstream receive-pack: %w", err)
	}
	return response, nil
}
