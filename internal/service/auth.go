package service

import (
	"context"
	"errors"
	"strings"

	"github.com/oberthci/oberth/internal/api"
)

type Authenticator struct {
	source UplinkAuthenticator
}

func NewAuthenticator(source UplinkAuthenticator) (*Authenticator, error) {
	if source == nil {
		return nil, errors.New("service: uplink authenticator is required")
	}
	return &Authenticator{source: source}, nil
}

func (authenticator *Authenticator) Authenticate(ctx context.Context, token string) (api.Actor, error) {
	authenticated, err := authenticator.source.Authenticate(ctx, token)
	if err != nil {
		return api.Actor{}, err
	}
	identity := strings.TrimSpace(authenticated.Identity)
	fingerprint := strings.TrimSpace(authenticated.Fingerprint)
	if identity == "" || fingerprint == "" {
		return api.Actor{}, errors.New("service: authenticated token has no unique uplink binding")
	}
	return api.Actor{Identity: identity, Fingerprint: fingerprint}, nil
}
