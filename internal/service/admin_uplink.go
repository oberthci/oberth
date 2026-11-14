package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/auth"
	"github.com/oberthci/oberth/internal/clientcert"
	"github.com/oberthci/oberth/internal/invite"
	"github.com/oberthci/oberth/internal/model"
)

type administrativeUplinkStore interface {
	auth.TokenRepository
	CreateTokenCredentialWithPolicy(context.Context, model.TokenCredentialSpec, model.UplinkTokenPolicy) (model.TokenCredential, error)
	RegisterCertificateUplink(context.Context, string, model.UplinkSpec) (model.Uplink, error)
	RegisterUplink(context.Context, string, model.UplinkSpec) (model.Uplink, error)
	ResolveUplinkScopes(context.Context, []string) ([]string, error)
}
type administrativeTokenIssuer struct {
	administrativeUplinkStore
	gate   func(context.Context) error
	policy model.UplinkTokenPolicy
}

func (s administrativeTokenIssuer) CreateTokenCredential(ctx context.Context, spec model.TokenCredentialSpec) (model.TokenCredential, error) {
	if err := s.gate(ctx); err != nil {
		return model.TokenCredential{}, err
	}
	return s.CreateTokenCredentialWithPolicy(ctx, spec, s.policy)
}
func (s administrativeTokenIssuer) ActivateTokenCredential(ctx context.Context, id string) (model.TokenCredential, error) {
	if err := s.gate(ctx); err != nil {
		return model.TokenCredential{}, err
	}
	return s.administrativeUplinkStore.ActivateTokenCredential(ctx, id)
}

func (s *API) adminUplinkAdd(ctx context.Context, actor api.Actor, raw json.RawMessage) (any, error) {
	var args api.UplinkAddRequest
	if err := decodeAdministration(raw, &args); err != nil {
		return nil, err
	}
	if invite.ValidateIdentity(args.Identity) != nil || len(args.SSHPublicKey) > 64<<10 {
		return nil, refuse(ErrInvalidInput, "invalid uplink identity or public key")
	}
	key, _, options, rest, err := ssh.ParseAuthorizedKey([]byte(args.SSHPublicKey))
	if err != nil || len(options) != 0 || len(bytes.TrimSpace(rest)) != 0 {
		return nil, refuse(ErrInvalidInput, "one bare SSH public key is required")
	}
	if args.CSR != "" {
		if len(args.CSR) > 64<<10 {
			return nil, refuse(ErrInvalidInput, "CSR exceeds 64 KiB")
		}
		if _, err := clientcert.ParseCSR([]byte(args.CSR)); err != nil {
			return nil, refuse(ErrInvalidInput, "invalid public CSR")
		}
		if s.clientCertificates == nil || s.clientCertificates.Authority == nil {
			return nil, refuse(ErrUnavailable, "client certificate authority")
		}
	}
	if args.Bearer && args.CSR == "" {
		return nil, refuse(ErrInvalidInput, "bearer is only needed alongside a CSR")
	}
	policy := s.administration.TokenPolicy
	if policy.DefaultTTL == 0 && policy.MaxTTL == 0 {
		policy = model.DefaultUplinkTokenPolicy()
	}
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	var lifetime time.Duration
	if args.TTL != "" {
		lifetime, err = time.ParseDuration(args.TTL)
		if err != nil || lifetime <= 0 || lifetime > policy.MaxTTL {
			return nil, refuse(ErrInvalidInput, "TTL exceeds the configured issuance policy")
		}
	}
	if args.CSR != "" && lifetime > 0 && lifetime < time.Hour {
		return nil, refuse(ErrInvalidInput, "certificate lifetime must be at least one hour")
	}
	if args.CSR != "" {
		if _, _, err := s.clientCertificates.Authority.Issue(args.Identity, []byte(args.CSR), lifetime); err != nil {
			return nil, refuse(ErrInvalidInput, "CSR lifetime is outside the client CA policy")
		}
	}
	if len(args.Scope) > 128 {
		return nil, refuse(ErrInvalidInput, "at most 128 upstream scopes are accepted")
	}
	for _, name := range args.Scope {
		if name != model.MetricsScope {
			if err := validateAdminUpstreamName(name); err != nil {
				return nil, fmt.Errorf("%w: invalid upstream scope", ErrInvalidInput)
			}
		}
	}
	database, ok := s.runs.(administrativeUplinkStore)
	if !ok {
		return nil, refuse(ErrUnavailable, "uplink enrollment")
	}
	scopes, err := database.ResolveUplinkScopes(ctx, args.Scope)
	if err != nil {
		return nil, err
	}
	if args.Admin && model.MetricsOnly(scopes) {
		return nil, refuse(ErrInvalidInput, "a metrics-only identity cannot be an admin")
	}
	fingerprint := ssh.FingerprintSHA256(key)
	spec := model.UplinkSpec{Identity: args.Identity, Fingerprint: fingerprint, Admin: args.Admin, Scope: scopes}
	result := api.UplinkAddResult{Identity: args.Identity, Fingerprint: fingerprint, Admin: args.Admin, Scope: append([]string{}, scopes...)}
	if args.CSR != "" && !args.Bearer {
		if err := s.requireMutation(ctx); err != nil {
			return nil, err
		}
		if _, err := database.RegisterCertificateUplink(ctx, actor.Identity, spec); err != nil {
			return nil, err
		}
	} else {
		var token bytes.Buffer
		defer func() { clear(token.Bytes()) }()
		issuer := auth.Issuer{Tokens: administrativeTokenIssuer{administrativeUplinkStore: database, gate: s.requireMutation, policy: policy}, Bind: func(ctx context.Context, pending model.TokenCredential) error {
			if err := s.requireMutation(ctx); err != nil {
				return err
			}
			bound := spec
			bound.TokenCredentialID = pending.ID
			_, err := database.RegisterUplink(ctx, actor.Identity, bound)
			return err
		}}
		if lifetime > 0 {
			expiry := time.Now().Add(lifetime)
			issuer.ExpiresAt = &expiry
		}
		if _, err := issuer.Issue(ctx, args.Identity, &token); err != nil {
			return nil, err
		}
		result.Token = strings.TrimSpace(token.String())
	}
	if args.CSR != "" {
		if err := s.requireMutation(ctx); err != nil {
			return nil, err
		}
		result.Certificate, err = IssueClientCert(ctx, s.clientCertificates, actor.Identity, args.Identity, fingerprint, []byte(args.CSR), lifetime)
		if err != nil {
			return nil, errors.Join(err, refuse(ErrUnavailable, "certificate enrollment did not complete"))
		}
	}
	return result, nil
}
