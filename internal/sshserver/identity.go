package sshserver

import (
	"context"
	"errors"
	"net"

	"golang.org/x/crypto/ssh"
)

// UnregisteredError is a completed identity lookup that found no usable key.
// Other lookup errors are temporary failures and never become ordinary denial.
type UnregisteredError struct{}

func (*UnregisteredError) Error() string { return reasonUnregistered + ". " + remedyRegister }
func (s *Server) handshakeConfig(ctx context.Context, conn net.Conn, identity *Identity) *ssh.ServerConfig {
	var preauth ssh.ServerPreAuthConn
	lookup := func(key ssh.PublicKey) (*ssh.Permissions, error) {
		value, err := s.lookup(ctx, key)
		var missing *UnregisteredError
		if errors.As(err, &missing) && preauth != nil {
			_ = preauth.SendAuthBanner(missing.Error() + "\n")
		}
		if err != nil && !errors.As(err, &missing) {
			if preauth != nil {
				_ = preauth.SendAuthBanner(reasonTemporary + ". " + remedyRetry + "\n")
			}
			_ = conn.Close()
		}
		if err == nil {
			*identity = value
		}
		return nil, err
	}
	cfg := &ssh.ServerConfig{PreAuthConnCallback: func(c ssh.ServerPreAuthConn) { preauth = c },
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) { return lookup(key) },
		VerifiedPublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey, _ *ssh.Permissions, _ string) (*ssh.Permissions, error) {
			return lookup(key)
		}}
	cfg.AddHostKey(s.config.Signer)
	return cfg
}
func (s *Server) lookup(ctx context.Context, key ssh.PublicKey) (Identity, error) {
	phase, cancel := context.WithTimeout(ctx, s.config.Handshake)
	defer cancel()
	value, err := s.config.Lookup(phase, key)
	if err != nil {
		return Identity{}, err
	}
	if value.Actor == "" || value.CanAccess == nil {
		return Identity{}, &UnregisteredError{}
	}
	return value, nil
}
