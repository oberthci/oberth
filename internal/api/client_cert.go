package api

import (
	"context"
	"crypto/x509"
	"net/http"
	"slices"
	"time"
)

type CertificateAuthenticator interface {
	AuthenticateCertificate(context.Context, *x509.Certificate) (Actor, error)
}

// authenticateRequest uses only TLS-verified leaves. A supplied but unknown
// certificate never falls back to a bearer, and dual credentials must agree.
func (s *Server) authenticateRequest(ctx context.Context, r *http.Request) (Actor, error) {
	var certActor Actor
	hasCert := r.TLS != nil && len(r.TLS.PeerCertificates) > 0
	if hasCert {
		if len(r.TLS.VerifiedChains) == 0 {
			return Actor{}, ErrInvalidToken
		}
		leaf := r.TLS.VerifiedChains[0][0]
		now := time.Now()
		if now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) {
			return Actor{}, ErrInvalidToken
		}
		verifier, ok := s.auth.(CertificateAuthenticator)
		if !ok {
			return Actor{}, ErrInvalidToken
		}
		var err error
		certActor, err = verifier.AuthenticateCertificate(ctx, leaf)
		if err != nil {
			return Actor{}, err
		}
		certActor.AuthMethod = "cert"
	}
	headers := r.Header.Values("Authorization")
	if hasCert && (len(headers) == 0 || len(headers) == 1 && (headers[0] == "Bearer " || headers[0] == "Bearer")) {
		return certActor, nil
	}
	token, ok := bearerToken(headers)
	if !ok {
		return Actor{}, ErrInvalidToken
	}
	actor, err := s.auth.Authenticate(ctx, token)
	if err != nil {
		return Actor{}, err
	}
	if hasCert {
		if actor.Identity != certActor.Identity || actor.Fingerprint != certActor.Fingerprint || actor.Admin != certActor.Admin || !slices.Equal(actor.Scope, certActor.Scope) {
			return Actor{}, ErrInvalidToken
		}
		return certActor, nil
	}
	actor.AuthMethod = "bearer"
	return actor, nil
}
