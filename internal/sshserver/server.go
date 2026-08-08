// Package sshserver exposes only Git's smart protocol over authenticated SSH.
package sshserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/oberthci/oberth/internal/gitcache"
)

const identityExtension = "oberth.identity"

const (
	defaultHandshakeTimeout         = 10 * time.Second
	defaultRequestTimeout           = 30 * time.Second
	defaultSessionTimeout           = 30 * time.Minute
	defaultMaxConnections           = 64
	defaultMaxSessions              = 64
	defaultMaxSessionsPerConnection = 8
)

// IdentityResolver binds an SSH public-key fingerprint to the durable uplink
// identity used for callbacks and audit.
type IdentityResolver interface {
	ResolveFingerprint(fingerprint string) (identity string, found bool, err error)
}

type IdentityResolverFunc func(string) (string, bool, error)

func (f IdentityResolverFunc) ResolveFingerprint(fingerprint string) (string, bool, error) {
	return f(fingerprint)
}

// UpdateHandler consumes durable, actor-bound receive events. Event IDs remain
// stable across retry and startup replay and must be used idempotently.
type UpdateHandler = gitcache.ReceiveHandler
type UpdateHandlerFunc = gitcache.ReceiveHandlerFunc

type Logger interface {
	Printf(string, ...any)
}

type discardLogger struct{}

func (discardLogger) Printf(string, ...any) {}

type Config struct {
	HostSigner               ssh.Signer
	Resolver                 IdentityResolver
	Git                      gitcache.ServiceRunner
	OnUpdate                 UpdateHandler
	Logger                   Logger
	MutationGate             func(context.Context) error
	HandshakeTimeout         time.Duration
	RequestTimeout           time.Duration
	SessionTimeout           time.Duration
	MaxConnections           int
	MaxSessions              int
	MaxSessionsPerConnection int
}

type Server struct {
	config                   *ssh.ServerConfig
	git                      gitcache.ServiceRunner
	onUpdate                 UpdateHandler
	logger                   Logger
	mutationGate             func(context.Context) error
	handshakeTimeout         time.Duration
	requestTimeout           time.Duration
	sessionTimeout           time.Duration
	connections              chan struct{}
	sessions                 chan struct{}
	maxSessionsPerConnection int
}

func New(config Config) (*Server, error) {
	if config.HostSigner == nil {
		return nil, errors.New("persistent SSH host signer is required")
	}
	if config.Resolver == nil {
		return nil, errors.New("SSH identity resolver is required")
	}
	if config.Git == nil {
		return nil, errors.New("git service runner is required")
	}
	if config.OnUpdate == nil {
		return nil, errors.New("durable receive update handler is required")
	}
	if config.HandshakeTimeout < 0 || config.RequestTimeout < 0 || config.SessionTimeout < 0 ||
		config.MaxConnections < 0 || config.MaxSessions < 0 || config.MaxSessionsPerConnection < 0 {
		return nil, errors.New("SSH limits must not be negative")
	}
	if config.HandshakeTimeout == 0 {
		config.HandshakeTimeout = defaultHandshakeTimeout
	}
	if config.RequestTimeout == 0 {
		config.RequestTimeout = defaultRequestTimeout
	}
	if config.SessionTimeout == 0 {
		config.SessionTimeout = defaultSessionTimeout
	}
	if config.MaxConnections == 0 {
		config.MaxConnections = defaultMaxConnections
	}
	if config.MaxSessions == 0 {
		config.MaxSessions = defaultMaxSessions
	}
	if config.MaxSessionsPerConnection == 0 {
		config.MaxSessionsPerConnection = defaultMaxSessionsPerConnection
	}
	logger := config.Logger
	if logger == nil {
		logger = discardLogger{}
	}
	mutationGate := config.MutationGate
	if mutationGate == nil {
		mutationGate = func(context.Context) error { return nil }
	}
	serverConfig := &ssh.ServerConfig{
		ServerVersion: "SSH-2.0-Oberth",
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			fingerprint := ssh.FingerprintSHA256(key)
			identity, found, err := config.Resolver.ResolveFingerprint(fingerprint)
			if err != nil {
				return nil, fmt.Errorf("resolve public key: %w", err)
			}
			identity = strings.TrimSpace(identity)
			if !found || identity == "" {
				return nil, errors.New("public key is not registered")
			}
			return &ssh.Permissions{Extensions: map[string]string{identityExtension: identity}}, nil
		},
	}
	serverConfig.AddHostKey(config.HostSigner)
	return &Server{
		config: serverConfig, git: config.Git, onUpdate: config.OnUpdate, logger: logger, mutationGate: mutationGate,
		handshakeTimeout:         config.HandshakeTimeout,
		requestTimeout:           config.RequestTimeout,
		sessionTimeout:           config.SessionTimeout,
		connections:              make(chan struct{}, config.MaxConnections),
		sessions:                 make(chan struct{}, config.MaxSessions),
		maxSessionsPerConnection: config.MaxSessionsPerConnection,
	}, nil
}

// ParseHostSigner loads the persistent private key supplied by a Kubernetes
// Secret. It never generates an ephemeral replacement.
func ParseHostSigner(privateKey []byte) (ssh.Signer, error) {
	signer, err := ssh.ParsePrivateKey(privateKey)
	if err != nil {
		return nil, fmt.Errorf("parse persistent SSH host key: %w", err)
	}
	return signer, nil
}

// Serve accepts connections until ctx ends or the listener fails.
func (s *Server) Serve(ctx context.Context, listener net.Listener) error {
	if listener == nil {
		return errors.New("SSH listener is required")
	}
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()
	var active sync.WaitGroup
	for {
		connection, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				active.Wait()
				return nil
			}
			return fmt.Errorf("accept SSH connection: %w", err)
		}
		if !tryAcquire(s.connections) {
			_ = connection.Close()
			continue
		}
		active.Add(1)
		go func() {
			defer active.Done()
			defer release(s.connections)
			if err := s.handleConn(ctx, connection); err != nil && ctx.Err() == nil {
				s.logger.Printf("SSH connection failed: %v", err)
			}
		}()
	}
}

// HandleConn serves one already accepted network connection.
func (s *Server) HandleConn(ctx context.Context, connection net.Conn) error {
	if connection == nil {
		return errors.New("SSH connection is required")
	}
	if !tryAcquire(s.connections) {
		_ = connection.Close()
		return errors.New("SSH connection limit reached")
	}
	defer release(s.connections)
	return s.handleConn(ctx, connection)
}

func (s *Server) handleConn(ctx context.Context, connection net.Conn) error {
	defer func() { _ = connection.Close() }()
	if err := connection.SetDeadline(time.Now().Add(s.handshakeTimeout)); err != nil {
		return fmt.Errorf("set SSH handshake deadline: %w", err)
	}
	stopClose := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = connection.Close()
		case <-stopClose:
		}
	}()
	defer close(stopClose)
	serverConn, channels, requests, err := ssh.NewServerConn(connection, s.config)
	if err != nil {
		return fmt.Errorf("SSH handshake: %w", err)
	}
	maximumDeadline := time.Now().Add(s.sessionTimeout)
	if err := connection.SetDeadline(minimumDeadline(time.Now().Add(s.requestTimeout), maximumDeadline)); err != nil {
		_ = serverConn.Close()
		return fmt.Errorf("set authenticated SSH request deadline: %w", err)
	}
	defer func() { _ = serverConn.Close() }()
	actor := serverConn.Permissions.Extensions[identityExtension]
	connectionCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go rejectGlobalRequests(requests)

	var activeSessions sync.WaitGroup
	connectionSessions := make(chan struct{}, s.maxSessionsPerConnection)
	for channel := range channels {
		if channel.ChannelType() != "session" {
			_ = channel.Reject(ssh.UnknownChannelType, "only session channels are allowed")
			continue
		}
		if !tryAcquire(s.sessions) {
			_ = channel.Reject(ssh.ResourceShortage, "server session limit reached")
			continue
		}
		if !tryAcquire(connectionSessions) {
			release(s.sessions)
			_ = channel.Reject(ssh.ResourceShortage, "connection session limit reached")
			continue
		}
		stream, channelRequests, err := channel.Accept()
		if err != nil {
			release(connectionSessions)
			release(s.sessions)
			continue
		}
		activeSessions.Add(1)
		go func() {
			defer activeSessions.Done()
			releaseSession := sync.OnceFunc(func() {
				release(connectionSessions)
				release(s.sessions)
			})
			defer releaseSession()
			s.handleSession(connectionCtx, actor, stream, channelRequests, func() error {
				return connection.SetDeadline(maximumDeadline)
			}, releaseSession)
		}()
	}
	cancel()
	activeSessions.Wait()
	return nil
}

func tryAcquire(slots chan struct{}) bool {
	select {
	case slots <- struct{}{}:
		return true
	default:
		return false
	}
}

func release(slots chan struct{}) { <-slots }

func rejectGlobalRequests(requests <-chan *ssh.Request) {
	for request := range requests {
		if request.WantReply {
			_ = request.Reply(false, nil)
		}
	}
}

func (s *Server) handleSession(
	ctx context.Context,
	actor string,
	channel ssh.Channel,
	requests <-chan *ssh.Request,
	beginExec func() error,
	complete func(),
) {
	defer func() { _ = channel.Close() }()
	defer complete()
	protocol := ""
	for request := range requests {
		switch request.Type {
		case "env":
			accepted := protocol == "" && parseProtocolEnv(request.Payload, &protocol)
			if request.WantReply {
				_ = request.Reply(accepted, nil)
			}
		case "exec":
			command, err := parseExecPayload(request.Payload)
			if err != nil {
				complete()
				if request.WantReply {
					_ = request.Reply(false, nil)
				}
				_, _ = io.WriteString(channel.Stderr(), "oberth: "+err.Error()+"\n")
				sendExitStatus(channel, 1)
				return
			}
			if beginExec == nil || beginExec() != nil {
				complete()
				if request.WantReply {
					_ = request.Reply(false, nil)
				}
				sendExitStatus(channel, 1)
				return
			}
			if request.WantReply {
				_ = request.Reply(true, nil)
			}
			if err := s.runGit(ctx, actor, protocol, command, channel); err != nil {
				complete()
				_, _ = io.WriteString(channel.Stderr(), "oberth: "+err.Error()+"\n")
				sendExitStatus(channel, 1)
				return
			}
			complete()
			sendExitStatus(channel, 0)
			return
		default:
			if request.WantReply {
				_ = request.Reply(false, nil)
			}
		}
	}
}

func minimumDeadline(left, right time.Time) time.Time {
	if left.Before(right) {
		return left
	}
	return right
}

func (s *Server) runGit(ctx context.Context, actor, protocol string, command gitCommand, channel ssh.Channel) error {
	if command.service == gitcache.ReceivePack {
		if err := s.mutationGate(ctx); err != nil {
			return fmt.Errorf("audit integrity is unavailable; push rejected: %w", err)
		}
		// Receive owns preparation under the same repository lock as durable
		// replay, reservation, ref publication, and callbacks. Refreshing here
		// could move refs before a crashed reservation is recovered.
		return s.git.Receive(ctx, command.repo, actor, protocol, channel, channel, channel.Stderr(), s.onUpdate)
	}

	repository, err := s.git.Ensure(ctx, command.repo)
	if err != nil {
		return fmt.Errorf("prepare repository: %w", err)
	}
	if repository.Stale {
		_, _ = io.WriteString(channel.Stderr(), "remote: oberth: upstream unavailable; serving cached repository\n")
	}
	return s.git.Serve(ctx, command.repo, command.service, protocol, channel, channel, channel.Stderr())
}

// ReplayPending delivers durable receive events left by an interrupted process
// or callback failure. Call it before Serve during process startup.
func (s *Server) ReplayPending(ctx context.Context) error {
	if err := s.mutationGate(ctx); err != nil {
		return fmt.Errorf("audit integrity is unavailable; receive replay blocked: %w", err)
	}
	return s.git.ReplayPending(ctx, s.onUpdate)
}

type gitCommand struct {
	service gitcache.Service
	repo    string
}

func parseExecPayload(payload []byte) (gitCommand, error) {
	var request struct{ Command string }
	if err := ssh.Unmarshal(payload, &request); err != nil {
		return gitCommand{}, errors.New("malformed exec request")
	}
	return ParseCommand(request.Command)
}

// ParseCommand accepts exactly the argv-free command shape emitted by Git's
// SSH transport. It is deliberately not a shell parser.
func ParseCommand(value string) (gitCommand, error) {
	if strings.ContainsAny(value, "\x00\r\n") {
		return gitCommand{}, errors.New("malformed Git command")
	}
	separator := strings.IndexByte(value, ' ')
	if separator <= 0 || separator == len(value)-1 {
		return gitCommand{}, errors.New("expected a Git smart-protocol command")
	}
	service, err := gitcache.ParseService(value[:separator])
	if err != nil {
		return gitCommand{}, err
	}
	quoted := value[separator+1:]
	if len(quoted) < 3 || quoted[0] != '\'' || quoted[len(quoted)-1] != '\'' || strings.Contains(quoted[1:len(quoted)-1], "'") {
		return gitCommand{}, errors.New("repository path must be one single-quoted argument")
	}
	repo, err := gitcache.NormalizeRepo(quoted[1 : len(quoted)-1])
	if err != nil {
		return gitCommand{}, err
	}
	return gitCommand{service: service, repo: repo}, nil
}

func parseProtocolEnv(payload []byte, result *string) bool {
	var request struct {
		Name  string
		Value string
	}
	if err := ssh.Unmarshal(payload, &request); err != nil || request.Name != "GIT_PROTOCOL" {
		return false
	}
	switch request.Value {
	case "version=0", "version=1", "version=2":
		*result = request.Value
		return true
	default:
		return false
	}
}

func sendExitStatus(channel ssh.Channel, status uint32) {
	_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{Status: status}))
}
