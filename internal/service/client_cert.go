package service

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/clientcert"
	"github.com/oberthci/oberth/internal/model"
)

type ClientCertStore interface {
	CertificateUplink(context.Context, string, string) (model.Uplink, error)
	BindClientPin(context.Context, string, int64, string, string, time.Time) error
}

type ClientCertIssuer struct {
	Authority *clientcert.Authority
	Store     ClientCertStore
}

// IssueClientCert is the shared issuance path for administrative enrollment,
// install, renewal and invite redemption. Callers authorize the target first.
// Only public CSR/certificate bytes cross this boundary. The pin is committed
// before the certificate is returned, and a revoked/deleted uplink is refused.
func IssueClientCert(ctx context.Context, issuer *ClientCertIssuer, actor, identity, fingerprint string, csr []byte, lifetime time.Duration) (string, error) {
	return issueClientCert(ctx, issuer, actor, identity, fingerprint, "", csr, lifetime)
}

func issueClientCert(ctx context.Context, issuer *ClientCertIssuer, actor, identity, fingerprint, credentialID string, csr []byte, lifetime time.Duration) (string, error) {
	if issuer == nil || issuer.Authority == nil || issuer.Store == nil {
		return "", refuse(ErrUnavailable, "client certificate authority")
	}
	u, err := issuer.Store.CertificateUplink(ctx, identity, fingerprint)
	if err != nil {
		return "", err
	}
	if credentialID != "" && u.TokenCredentialID != credentialID {
		return "", refuse(ErrForbidden, "client credential was replaced")
	}
	cert, leaf, err := issuer.Authority.Issue(u.Identity, csr, lifetime)
	if err != nil {
		return "", errors.Join(ErrInvalidInput, err)
	}
	if err := issuer.Store.BindClientPin(ctx, actor, u.ID, u.TokenCredentialID, clientcert.Pin(leaf), leaf.NotAfter); err != nil {
		return "", err
	}
	return string(cert), nil
}

func (s *API) clientCertTool(ctx context.Context, actor api.Actor, name string, raw json.RawMessage) (any, error) {
	var args struct {
		Identity    string `json:"identity"`
		Fingerprint string `json:"fingerprint"`
		CSR         string `json:"csr"`
		TTL         string `json:"ttl"`
	}
	if err := decodeTool(raw, &args); err != nil {
		return nil, err
	}
	credentialID := ""
	if name == "client_renew" {
		if actor.AuthMethod != "cert" || actor.TokenCredentialID == "" {
			return nil, refuse(ErrForbidden, "renewal requires a current client certificate")
		}
		if args.Identity != "" || args.Fingerprint != "" {
			return nil, refuse(ErrInvalidInput, "renewal uses the authenticated uplink")
		}
		args.Identity = actor.Identity
		args.Fingerprint = actor.Fingerprint
		credentialID = actor.TokenCredentialID
	} else if !actor.Admin || actor.Scoped() {
		return nil, refuse(ErrForbidden, "uplink_cert requires unscoped admin")
	}
	lifetime := time.Duration(0)
	if args.TTL != "" {
		var err error
		lifetime, err = time.ParseDuration(args.TTL)
		if err != nil {
			return nil, refuse(ErrInvalidInput, "invalid certificate lifetime")
		}
	}
	cert, err := issueClientCert(ctx, s.clientCertificates, actor.Identity, args.Identity, args.Fingerprint, credentialID, []byte(args.CSR), lifetime)
	if err != nil {
		return nil, err
	}
	return map[string]string{"certificate": cert}, nil
}
