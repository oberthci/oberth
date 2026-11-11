package service

import (
	"context"
	"crypto/x509"
	"errors"
	"strings"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/auth"
	"github.com/oberthci/oberth/internal/clientcert"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

type Authenticator struct {
	source       UplinkAuthenticator
	Certificates interface {
		UplinkByClientPin(context.Context, string) (model.Uplink, error)
	}
}

func NewAuthenticator(source UplinkAuthenticator) (*Authenticator, error) {
	if source == nil {
		return nil, errors.New("service: uplink authenticator is required")
	}
	return &Authenticator{source: source}, nil
}

func (authenticator *Authenticator) Authenticate(ctx context.Context, token string) (api.Actor, error) {
	authenticated, err := authenticator.source.Authenticate(ctx, token)
	if errors.Is(err, auth.ErrInvalidToken) {
		return api.Actor{}, api.ErrInvalidToken
	}
	if err != nil {
		return api.Actor{}, err
	}
	identity := strings.TrimSpace(authenticated.Identity)
	fingerprint := strings.TrimSpace(authenticated.Fingerprint)
	if identity == "" || fingerprint == "" {
		return api.Actor{}, errors.New("service: authenticated token has no unique uplink binding")
	}
	return api.Actor{Identity: identity, Fingerprint: fingerprint, Admin: authenticated.Admin, TokenCredentialID: authenticated.TokenCredential.ID,
		Scope: append([]string(nil), authenticated.Scope...)}, nil
}

func (a *Authenticator) AuthenticateCertificate(ctx context.Context, cert *x509.Certificate) (api.Actor, error) {
	if a.Certificates == nil {
		return api.Actor{}, api.ErrInvalidToken
	}
	u, err := a.Certificates.UplinkByClientPin(ctx, clientcert.Pin(cert))
	if errors.Is(err, store.ErrNotFound) {
		return api.Actor{}, api.ErrInvalidToken
	}
	if err != nil {
		return api.Actor{}, err
	}
	return api.Actor{Identity: u.Identity, Fingerprint: u.Fingerprint, TokenCredentialID: u.TokenCredentialID, Admin: u.Admin, Scope: u.Scope, AuthMethod: "cert"}, nil
}
