package api

import (
	"context"
	"github.com/oberthci/oberth/internal/apicontract"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/publicerr"
)

func (s *Server) authenticate(r *http.Request) (apicontract.Caller, error) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	c, err := s.credentials(ctx, r)
	if ctx.Err() != nil {
		return apicontract.Caller{}, refusal("authbusy")
	}
	if err != nil {
		if s.config.Classify(err).Code() != publicerr.Forbidden {
			return apicontract.Caller{}, refusal("authbusy")
		}
		return apicontract.Caller{}, refusal("auth")
	}
	if c.CredentialID == "" || c.Actor.Identity == "" || c.Actor.AuthMethod == "" {
		return apicontract.Caller{}, refusal("authbusy")
	}
	c.Actor = c.Actor.Clone()
	return c, nil
}
func (s *Server) credentials(ctx context.Context, r *http.Request) (apicontract.Caller, error) {
	var cert apicontract.Caller
	hasCert := r.TLS != nil && len(r.TLS.PeerCertificates) > 0
	if hasCert {
		var err error
		cert, err = s.certificate(ctx, r)
		if err != nil {
			return apicontract.Caller{}, err
		}
	}
	token, err := bearerToken(r, hasCert)
	if err != nil {
		return apicontract.Caller{}, err
	}
	if token == "" {
		return cert, nil
	}
	bearer, err := s.config.Auth.Bearer(ctx, token)
	if err != nil {
		return apicontract.Caller{}, err
	}
	bearer.Actor.AuthMethod = "bearer"
	if hasCert {
		if !sameAuthority(cert, bearer) {
			return apicontract.Caller{}, refusal("auth")
		}
		return cert, nil
	}
	return bearer, nil
}
func sameAuthority(a, b apicontract.Caller) bool {
	return a.Actor.Identity == b.Actor.Identity && a.Actor.Fingerprint == b.Actor.Fingerprint && a.Actor.Admin == b.Actor.Admin && slices.Equal(a.Actor.Scope, b.Actor.Scope)
}

func (s *Server) certificate(ctx context.Context, r *http.Request) (apicontract.Caller, error) {
	if len(r.TLS.VerifiedChains) == 0 || len(r.TLS.VerifiedChains[0]) == 0 {
		return apicontract.Caller{}, refusal("auth")
	}
	leaf := r.TLS.VerifiedChains[0][0]
	if s.config.Runtime.Now().Before(leaf.NotBefore) || !s.config.Runtime.Now().Before(leaf.NotAfter) {
		return apicontract.Caller{}, refusal("auth")
	}
	c, err := s.config.Auth.Certificate(ctx, leaf)
	c.Actor.AuthMethod = "cert"
	return c, err
}

func bearerToken(r *http.Request, hasCert bool) (string, error) {
	headers := r.Header.Values("Authorization")
	if hasCert && len(headers) == 0 {
		return "", nil
	}
	if len(headers) != 1 || !strings.HasPrefix(headers[0], "Bearer ") {
		return "", refusal("auth")
	}
	token := strings.TrimPrefix(headers[0], "Bearer ")
	if token == "" || strings.ContainsAny(token, " \r\n\t,") {
		return "", refusal("auth")
	}
	return token, nil
}

func (s *Server) stillAuthorized(r *http.Request, c apicontract.Caller) bool {
	current, err := s.authenticate(r)
	return err == nil && current.CredentialID == c.CredentialID && sameAuthority(c, current)
}
