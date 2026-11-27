package sshserver

import (
	"context"
	"io"
	"net"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

type Identity struct {
	Actor, Fingerprint string
	CanAccess          func(string) bool
}
type Session struct {
	Identity            Identity
	Command             Command
	Input               io.ReadCloser
	Output, Diagnostics io.Writer
	Begin, End          func()
}
type Config struct {
	Signer ssh.Signer
	Lookup func(context.Context, ssh.PublicKey) (Identity, error)
	Run    func(context.Context, Session) error
	// Refused records authenticated scope denials before Run can own the request.
	Refused                func(context.Context, Session, error) error
	Now                    func() time.Time
	Handshake, Idle, Total time.Duration
	Connections            int
	UnauthenticatedPerPeer int
}
type Server struct {
	config          Config
	slots           chan struct{}
	peerMu          sync.Mutex
	unauthenticated map[string]int
}

func New(c Config) (*Server, error) {

	if c.Signer == nil || c.Lookup == nil || c.Run == nil || c.Now == nil {
		return nil, failure(reasonConfig)
	}
	if c.Handshake <= 0 {
		c.Handshake = 5 * time.Second
	}
	if c.Idle <= 0 {
		c.Idle = 30 * time.Second
	}
	if c.Total <= 0 {
		c.Total = 30 * time.Minute
	}
	if c.Connections <= 0 {
		c.Connections = 64
	}
	if c.Connections < 2 {
		return nil, failure(reasonConfig)
	}
	if c.UnauthenticatedPerPeer <= 0 || c.UnauthenticatedPerPeer > c.Connections/2 {
		c.UnauthenticatedPerPeer = c.Connections / 2
	}
	return &Server{config: c, slots: make(chan struct{}, c.Connections), unauthenticated: make(map[string]int)}, nil
}
func (s *Server) Serve(ctx context.Context, listener net.Listener) error {
	ctx, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	stop := context.AfterFunc(ctx, func() { _ = listener.Close() })
	defer func() { cancel(); _ = listener.Close(); workers.Wait(); stop() }()
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return context.Cause(ctx)
			}
			return err
		}
		workers.Add(1)
		go func() { defer workers.Done(); _ = s.Handle(ctx, conn) }()
	}
}

// Handle is also usable with an in-memory net.Pipe for transport contracts.
func (s *Server) Handle(ctx context.Context, conn net.Conn) error {
	release, ok := s.takeHandshake(conn.RemoteAddr())
	if !ok {
		_ = conn.Close()
		return failure(reasonIdentity)
	}
	select {
	case s.slots <- struct{}{}:
	default:
		release()
		_ = conn.Close()
		return failure(reasonIdentity)
	}
	defer func() { <-s.slots }()
	return s.handle(ctx, conn, release)
}

func (s *Server) handle(ctx context.Context, conn net.Conn, release func()) error {
	defer release()
	defer func() { _ = conn.Close() }()
	ctx, cancel := context.WithTimeout(ctx, s.config.Total)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	var identity Identity
	native := s.handshakeConfig(ctx, conn, &identity)
	if err := conn.SetDeadline(s.config.Now().Add(s.config.Handshake)); err != nil {
		return err
	}
	session, channels, requests, err := ssh.NewServerConn(conn, native)
	if err != nil {
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		return failure(reasonIdentity)
	}
	release()
	defer func() { _ = session.Close() }()
	deadline, _ := ctx.Deadline()
	if err = conn.SetDeadline(deadline); err != nil {
		return err
	}
	waiting := time.AfterFunc(s.config.Idle, func() { _ = conn.Close() })
	defer waiting.Stop()
	done := make(chan struct{})
	go func() { ssh.DiscardRequests(requests); close(done) }()
	defer func() { _ = session.Close(); <-done }()
	for channel := range channels {
		if channel.ChannelType() != "session" {
			_ = channel.Reject(ssh.UnknownChannelType, reasonCommand)
			continue
		}
		waiting.Stop()
		stream, reqs, err := channel.Accept()
		if err != nil {
			return err
		}
		s.session(ctx, peer{identity: identity, abort: func() { _ = conn.Close() }}, stream, reqs)
		waiting.Reset(s.config.Idle)
	}
	return nil
}

type peer struct {
	identity Identity
	abort    func()
}

func (s *Server) session(ctx context.Context, caller peer, ch ssh.Channel, requests <-chan *ssh.Request) {
	defer func() { _ = ch.Close() }()
	waiting := time.AfterFunc(s.config.Idle, caller.abort)
	defer waiting.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case request, ok := <-requests:
			if !ok {
				return
			}
			if request.Type != "exec" {
				_ = request.Reply(false, nil)
				continue
			}
			var payload struct{ Command string }
			if ssh.Unmarshal(request.Payload, &payload) != nil {
				_ = request.Reply(false, nil)
				return
			}
			cmd, err := ParseCommand(payload.Command)
			if err != nil {
				_, _ = io.WriteString(ch.Stderr(), gitFailure(err)+"\n")
				_ = request.Reply(false, nil)
				return
			}
			if !caller.identity.CanAccess(cmd.Repository) {
				_, _ = io.WriteString(ch.Stderr(), gitFailure(s.refuseScope(ctx, caller.identity, cmd))+"\n")
				_ = request.Reply(false, nil)
				return
			}
			_ = request.Reply(true, nil)
			waiting.Stop()
			s.execute(ctx, caller, cmd, ch)
			return
		}
	}
}
func (s *Server) execute(ctx context.Context, caller peer, cmd Command, ch ssh.Channel) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	idle := newIdle(ch, s.config.Idle, func() { cancel(); caller.abort() }, s.config.Now)
	defer idle.close()
	err := s.config.Run(ctx, Session{Identity: caller.identity, Command: cmd, Input: idle, Output: idle, Diagnostics: idle.stderr(), Begin: idle.begin, End: idle.end})
	code := uint32(0)
	if err != nil {
		code = 1
		if text := gitFailure(err); text != "" {
			_, _ = io.WriteString(ch.Stderr(), text+"\n")
		}

	}
	_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{code}))
}
