package main

import (
	"crypto/sha256"
	"errors"
	"io/fs"
	"path/filepath"

	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/publicerr"
)

type logIdentity struct {
	digest      [32]byte
	authority   [32]byte
	profile     string
	reconnected bool
}

// Only log reads may repeat after reloading an identity. The cursor belongs to
// the caller and advances only after a successful reply.
func (s *flowSession) invokeLog(name string, args, out any) error {
	if err := s.connect(); err != nil {
		return err
	}
	if s.logIdentity == nil {
		if err := s.rememberLogIdentity(); err != nil {
			return err
		}
	}
	data, readErr := logCertificate(s.resolved.ClientCert)
	changed := readErr != nil || sha256.Sum256(data) != s.logIdentity.digest
	if changed {
		return s.reconnectLog(name, args, out)
	}
	err := s.invoke(name, args, out)
	if err != nil && s.ctx.Err() == nil && (logAuthenticationFailed(err) || errors.Is(err, fs.ErrNotExist)) {
		return s.reconnectLog(name, args, out)
	}
	return err
}

func (s *flowSession) rememberLogIdentity() error {
	identity := &logIdentity{}
	if s.resolved.ClientCert != "" {
		dir := filepath.Dir(s.resolved.ClientCert)
		if filepath.Dir(dir) == s.app.base {
			identity.profile = filepath.Base(dir)
		}
	}
	ca, err := logCertificate(s.resolved.CAFile)
	if err != nil {
		return err
	}
	identity.authority = sha256.Sum256(ca)
	data, err := logCertificate(s.resolved.ClientCert)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	identity.digest = sha256.Sum256(data)
	s.logIdentity = identity
	return nil
}

func logCertificate(path string) ([]byte, error) {
	if path == "" {
		return nil, nil
	}
	return client.ReadPublic(path)
}

func logAuthenticationFailed(err error) bool {
	p := publicerr.Classify(err)
	return p.Code() == publicerr.Forbidden || p.Reason() == "Server refused this client certificate."
}

func (s *flowSession) reconnectLog(name string, args, out any) error {
	fail := func(cause error) error {
		return publicerr.New(publicerr.Unavailable, "Log connection could not be renewed.", "Re-run the command.", cause)
	}
	identity := s.logIdentity
	if identity.reconnected {
		return fail(nil)
	}
	identity.reconnected = true
	// Pin the original selection, endpoint and trust. Switching current profiles
	// must never move a running reader to another installation or identity.
	env := func(name string) string {
		if name == "OBERTH_PROFILE" && identity.profile != "" {
			return identity.profile
		}
		return s.app.env(name)
	}
	o := s.options.transport
	o.URL, o.CAFile = s.resolved.URL, s.resolved.CAFile
	resolved, err := client.Resolve(o, s.app.base, env)
	if err != nil {
		return fail(err)
	}
	data, err := logCertificate(resolved.ClientCert)
	if err != nil {
		return fail(err)
	}
	ca, err := logCertificate(resolved.CAFile)
	if err != nil || sha256.Sum256(ca) != identity.authority {
		return fail(err)
	}
	s.close()
	s.remote = nil
	cfg := s.app.config
	cfg.Options, cfg.Diagnostic = resolved, s.app.diagnostic
	s.remote, err = client.New(cfg)
	if err != nil {
		return fail(err)
	}
	s.resolved, identity.digest = resolved, sha256.Sum256(data)
	if err = s.invoke(name, args, out); err != nil {
		return fail(err)
	}
	return nil
}
