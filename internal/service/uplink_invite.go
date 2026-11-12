package service

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/invite"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

type UplinkInviteStore interface {
	CreateUplinkInvite(context.Context, string, string, string, string, string, []string, time.Duration) (invite.Binding, error)
	RedeemUplinkInvite(context.Context, string, string, string, string, func(*store.InviteEnrollment) (string, error)) (invite.Binding, string, error)
}

func (s *API) inviteStore() (UplinkInviteStore, error) {
	if s.clientCertificates == nil || s.clientCertificates.Authority == nil || invite.ValidatePin(s.clientCertificates.ServerCAPin) != nil {
		return nil, refuse(ErrUnavailable, "invite enrollment is not configured")
	}
	database, ok := s.clientCertificates.Store.(UplinkInviteStore)
	if !ok {
		return nil, refuse(ErrUnavailable, "invite enrollment is not configured")
	}
	return database, nil
}
func (s *API) uplinkInviteTool(ctx context.Context, actor api.Actor, raw json.RawMessage) (any, error) {
	if !actor.Admin || actor.Scoped() {
		return nil, refuse(ErrForbidden, "uplink_invite requires unscoped admin")
	}
	var args struct {
		Identity     string   `json:"identity"`
		ServerURL    string   `json:"server_url"`
		SecretDigest string   `json:"secret_digest"`
		Scope        []string `json:"scope"`
		Expires      string   `json:"expires"`
	}
	if err := decodeTool(raw, &args); err != nil {
		return nil, err
	}
	if invite.ValidateIdentity(args.Identity) != nil || invite.ValidateURL(args.ServerURL) != nil || invite.ValidatePin(args.SecretDigest) != nil {
		return nil, refuse(ErrInvalidInput, "invalid invite identity, server URL or credential digest")
	}
	ttl := invite.DefaultLifetime
	if args.Expires != "" {
		var err error
		ttl, err = time.ParseDuration(args.Expires)
		if err != nil {
			return nil, refuse(ErrInvalidInput, "invalid invite lifetime")
		}
	}
	if ttl <= 0 || ttl > invite.MaximumLifetime {
		return nil, refuse(ErrInvalidInput, "invite lifetime must be positive and at most 168h")
	}
	database, err := s.inviteStore()
	if err != nil {
		return nil, err
	}
	return database.CreateUplinkInvite(ctx, actor.Identity, args.SecretDigest, args.Identity, args.ServerURL, s.clientCertificates.ServerCAPin, args.Scope, ttl)
}

type InviteRedemption struct {
	Secret       string `json:"secret"`
	ServerURL    string `json:"server_url"`
	CAPin        string `json:"ca_pin"`
	CSR          string `json:"csr"`
	SSHPublicKey string `json:"ssh_public_key"`
}
type InviteCertificate struct {
	Identity       string   `json:"identity"`
	Scope          []string `json:"scope"`
	SSHFingerprint string   `json:"ssh_fingerprint"`
	Certificate    string   `json:"certificate"`
}

// RedeemInvite is the only unauthenticated enrollment operation. Its caller
// exposes no ordinary tools. The credential is checked inside the transaction;
// it is never an Actor, a bearer credential, an audit field or an error value.
func (s *API) RedeemInvite(ctx context.Context, raw json.RawMessage) (any, error) {
	if err := s.requireMutation(ctx); err != nil {
		return nil, err
	}
	var args InviteRedemption
	if err := decodeTool(raw, &args); err != nil {
		return nil, refuse(ErrInvalidInput, "invalid invite redemption request")
	}
	digest, err := invite.SecretDigest(args.Secret)
	if err != nil || invite.ValidateURL(args.ServerURL) != nil {
		return nil, refuse(ErrInvalidInput, "invalid invite redemption request")
	}
	database, err := s.inviteStore()
	if err != nil {
		return nil, err
	}
	if args.CAPin != s.clientCertificates.ServerCAPin {
		return nil, refuse(ErrForbidden, "invite was refused")
	}
	fingerprint, err := invite.SSHFingerprint([]byte(args.SSHPublicKey))
	if err != nil {
		return nil, refuse(ErrInvalidInput, "invalid SSH public key")
	}
	ctx = model.WithAuthMethod(ctx, "invite")
	// Authenticate and reserve the invitation before the shared issuer verifies
	// a CSR signature; random unauthenticated requests cannot trigger that work.
	binding, certificate, err := database.RedeemUplinkInvite(ctx, digest, args.ServerURL, args.CAPin, fingerprint, func(enrollment *store.InviteEnrollment) (string, error) {
		u, err := enrollment.Uplink()
		if err != nil {
			return "", err
		}
		return IssueClientCert(ctx, &ClientCertIssuer{Authority: s.clientCertificates.Authority, Store: enrollment}, u.Identity, u.Identity, u.Fingerprint, []byte(args.CSR), 0)
	})
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrInvalidState) {
		return nil, refuse(ErrForbidden, "invite was refused")
	}
	if err != nil {
		return nil, err
	}
	return InviteCertificate{Identity: binding.Identity, Scope: binding.Scope, SSHFingerprint: fingerprint, Certificate: certificate}, nil
}
