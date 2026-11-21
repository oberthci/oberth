package service

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/clientcert"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/reponame"
	"github.com/oberthci/oberth/internal/stateport"
	"golang.org/x/crypto/ssh"
)

func publicIdentity(identity string, scope []string) error {
	var errs []error
	if identity == "" || len(identity) > 256 || strings.TrimSpace(identity) != identity || strings.ContainsFunc(identity, unicode.IsControl) {
		errs = append(errs, bad("identity"))
	}
	for _, name := range scope {
		if reponame.ValidateUpstreamName(name) != nil {
			errs = append(errs, publicerr.New(publicerr.InvalidInput, "Uplink scope must name upstreams.", "Use an upstream name, such as origin; it grants every repository in that upstream.", nil))
		}
	}
	return errors.Join(errs...)
}
func lifetime(raw string) (time.Duration, error) {
	if raw == "" {
		return 90 * 24 * time.Hour, nil
	}
	d, e := time.ParseDuration(raw)
	if e != nil || d < time.Hour || d > 365*24*time.Hour {
		return 0, bad("ttl")
	}
	return d, nil
}
func publicSSHKey(raw []byte) (ssh.PublicKey, error) {
	if len(raw) > 64<<10 {
		return nil, bad("ssh_public_key")
	}
	key, _, options, rest, e := ssh.ParseAuthorizedKey(raw)
	if e != nil || len(rest) > 0 || len(options) > 0 {
		return nil, bad("ssh_public_key")
	}
	return key, nil
}
func sshFingerprint(raw string) (string, error) {
	key, e := publicSSHKey([]byte(raw))
	if e != nil {
		return "", e
	}
	return ssh.FingerprintSHA256(key), nil
}
func (s *Service) credential(key stateport.Key, csr, ttl, bearerDigest string) (stateport.IdentityRegistration, apicontract.Identity, error) {
	v := stateport.IdentityRegistration{Key: key}
	out := apicontract.Identity{Identity: key.Identity, Fingerprint: key.Fingerprint, Admin: key.Admin, Scope: publicScope(key.Scope)}
	d, e := lifetime(ttl)
	if e != nil {
		return v, out, e
	}
	v.Key.ExpiresAt = s.c.Now().Add(d)
	if csr != "" {
		if _, e = clientcert.ParseCSR([]byte(csr)); e != nil {
			return v, out, e
		}
		raw, leaf, err := s.c.People.Issuer.Issue(key.Identity, []byte(csr), d)
		if err != nil {
			return v, out, err
		}
		v.Key.ExpiresAt = leaf.NotAfter
		v.SPKI = base64.StdEncoding.EncodeToString(leaf.RawSubjectPublicKeyInfo)
		out.Certificate = string(raw)
	}
	if bearerDigest != "" {
		decoded, err := hex.DecodeString(bearerDigest)
		if err != nil || len(decoded) != sha256.Size || hex.EncodeToString(decoded) != bearerDigest {
			return v, out, bad("bearer_digest")
		}
		v.Credential = &stateport.Credential{Digest: bearerDigest, Fingerprint: key.Fingerprint, ExpiresAt: v.Key.ExpiresAt}
	}
	return v, out, nil
}
func secretDigest(secret string) string {
	d := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(d[:])
}
func (s *Service) PrepareUplinkAdd(ctx context.Context, c apicontract.Caller, q apicontract.UplinkAddArgs) (apicontract.Call[apicontract.Identity], error) {
	a, e := s.peopleAllow(ctx, c.Actor, "uplink_add")
	if e != nil {
		return nil, e
	}
	if e = publicIdentity(q.Identity, q.Scope); e != nil {
		return nil, e
	}
	fingerprint, e := uplinkFingerprint(q)
	if e != nil {
		return nil, e
	}
	return bounded(func(ctx context.Context) (apicontract.Identity, error) {
		if _, err := s.requireUpstreams(ctx, q.Scope); err != nil {
			return apicontract.Identity{}, err
		}
		v, out, err := s.credential(stateport.Key{Identity: q.Identity, Fingerprint: fingerprint, Scope: publicScope(q.Scope), Admin: q.Admin, Kind: "agent", ParkOnPush: true}, q.CSR, q.TTL, q.BearerDigest)
		if err != nil {
			return apicontract.Identity{}, err
		}
		if err = s.c.People.Enrollment.RegisterIdentity(ctx, s.peopleAction(a, "identity.changed", q.Reason, ""), v); err != nil {
			return apicontract.Identity{}, err
		}
		return out, nil
	}), nil
}

func uplinkFingerprint(q apicontract.UplinkAddArgs) (string, error) {
	if q.SshPublicKey != "" {
		return sshFingerprint(q.SshPublicKey)
	}
	if q.BearerDigest == "" {
		return "", publicerr.New(publicerr.InvalidInput, "An uplink needs an SSH key or a browser bearer.", "Pass --ssh-public-key for Git pushes or --bearer-file for browser sign-in.", nil)
	}
	// SSH authentication only derives SHA256: fingerprints from public keys.
	return "bearer:" + secretDigest(q.Identity), nil
}
func (s *Service) selectedKey(ctx context.Context, identity, fingerprint string) (stateport.Key, error) {
	if fingerprint != "" {
		v, e := s.c.People.Enrollment.Key(ctx, fingerprint)
		if e == nil && (v.Identity != identity || v.Revoked) {
			return stateport.Key{}, refuse(missing)
		}
		return v, e
	}
	keys, e := s.c.People.Enrollment.IdentityKeys(ctx, "")
	if e != nil {
		return stateport.Key{}, e
	}
	var found []stateport.Key
	for _, k := range keys {
		if k.Identity == identity && !k.Revoked {
			found = append(found, k)
		}
	}
	if len(found) == 0 {
		return stateport.Key{}, refuse(missing)
	}
	if len(found) > 1 {
		return stateport.Key{}, refuse(ambiguous)
	}
	return found[0], nil
}
func (s *Service) issueCertificate(ctx context.Context, c apicontract.Caller, q apicontract.UplinkCertArgs, action string) (apicontract.Call[apicontract.Identity], error) {
	a, e := s.peopleAllow(ctx, c.Actor, action)
	if e != nil {
		return nil, e
	}
	key, e := s.selectedKey(ctx, q.Identity, q.Fingerprint)
	if e != nil {
		return nil, e
	}
	return bounded(func(ctx context.Context) (apicontract.Identity, error) {
		v, out, err := s.credential(key, q.CSR, q.TTL, "")
		if err != nil {
			return apicontract.Identity{}, err
		}
		if err = s.c.People.Enrollment.AddCertificatePin(ctx, s.peopleAction(a, "identity.changed", q.Reason, ""), stateport.CertificateBinding{Fingerprint: key.Fingerprint, SPKI: v.SPKI, ExpiresAt: v.Key.ExpiresAt}); err != nil {
			return apicontract.Identity{}, err
		}
		return out, nil
	}), nil
}
func (s *Service) PrepareUplinkCert(ctx context.Context, c apicontract.Caller, q apicontract.UplinkCertArgs) (apicontract.Call[apicontract.Identity], error) {
	return s.issueCertificate(ctx, c, q, "uplink_cert")
}
func (s *Service) PrepareClientRenew(ctx context.Context, c apicontract.Caller, q apicontract.ClientRenewArgs) (apicontract.Call[apicontract.Identity], error) {
	a, e := s.peopleAllow(ctx, c.Actor, "client_renew")
	if e != nil {
		return nil, e
	}
	if a.AuthMethod != "cert" {
		return nil, refuse(forbidden)
	}
	return s.issueCertificate(ctx, apicontract.Caller{Actor: a}, apicontract.UplinkCertArgs{Identity: a.Identity, Fingerprint: a.Fingerprint, CSR: q.CSR, TTL: q.TTL, Reason: q.Reason}, "client_renew")
}
func (s *Service) PrepareUplinkInvite(ctx context.Context, c apicontract.Caller, q apicontract.UplinkInviteArgs) (apicontract.Call[model.Invitation], error) {
	a, e := s.peopleAllow(ctx, c.Actor, "uplink_invite")
	if e != nil {
		return nil, e
	}
	if e = publicIdentity(q.Identity, q.Scope); e != nil {
		return nil, e
	}
	if e = invitationTarget(q, s.c.People.PublicURL); e != nil {
		return nil, e
	}
	d := 24 * time.Hour
	if q.Expires != "" {
		d, e = time.ParseDuration(q.Expires)
		if e != nil || d <= 0 || d > 7*24*time.Hour {
			return nil, bad("expires")
		}
	}
	v := model.Invitation{ID: s.c.NewID(), Identity: q.Identity, Scope: publicScope(q.Scope), ServerURL: q.ServerURL, CAPin: s.c.People.CAPin, ExpiresAt: s.c.Now().Add(d)}
	return bounded(func(ctx context.Context) (model.Invitation, error) {
		if _, err := s.requireUpstreams(ctx, q.Scope); err != nil {
			return model.Invitation{}, err
		}
		e := s.c.People.Enrollment.Invite(ctx, s.peopleAction(a, "invitation.created", q.Reason, ""), stateport.Invitation{Public: v, Digest: q.SecretDigest})
		return v, e
	}), nil
}

// Redeem checks the public address before reserving and parsing caller public keys. The CLI
// supplies a generated CSR, SSH public key and private handoff; no openssl step.
func (s *Service) Redeem(ctx context.Context, id string, q model.InviteRedemption) (model.InviteCertificate, error) {
	var out model.InviteCertificate
	if s.ctx.Err() != nil {
		return out, refuse(unavailable)
	}
	if len(q.Secret) < apicontract.InvitationSecretMinChars || len(q.Secret) > 512 {
		return out, refuse(invitationRefused)
	}
	if !apicontract.SameServerAddress(q.ServerURL, s.c.People.PublicURL) {
		return out, refuse(invitationAddressChanged)
	}
	token := s.c.NewID()
	v, e := s.c.People.Enrollment.ReserveInvitation(ctx, stateport.InvitationReservation{ID: id, Digest: secretDigest(q.Secret), Token: token, Until: s.c.Now().Add(time.Minute)})
	if e != nil {
		if publicerr.Classify(e).Code() == publicerr.Forbidden {
			return out, refuse(invitationRefused)
		}
		return out, e
	}
	if e = s.validateRedemption(ctx, v, q); e != nil {
		return out, e
	}
	fingerprint, e := sshFingerprint(q.SSHPublicKey)
	if e != nil {
		return out, e
	}
	registration, issued, e := s.credential(stateport.Key{Identity: v.Identity, Fingerprint: fingerprint, Scope: publicScope(v.Scope), Kind: "agent", ParkOnPush: true}, q.CSR, "", "")
	if e != nil {
		return out, e
	}
	if registration.SPKI == "" {
		return out, bad("csr")
	}
	a := stateport.Enrollment{Reservation: token, InvitationID: id, Digest: secretDigest(q.Secret), SPKI: registration.SPKI, Key: registration.Key, Credential: stateport.Credential{}, Events: s.peopleAction(actor.Actor{Identity: v.Identity, AuthMethod: "cert"}, "identity.changed", "", "").Events}
	if e = s.c.People.Enrollment.Enroll(ctx, a); e != nil {
		return out, e
	}
	return model.InviteCertificate{Identity: v.Identity, Scope: publicScope(v.Scope), SSHFingerprint: fingerprint, Certificate: issued.Certificate, SSHURL: s.c.People.SSHURL}, nil
}

func (s *Service) validateRedemption(ctx context.Context, v model.Invitation, q model.InviteRedemption) error {
	if q.ServerURL != v.ServerURL || q.CAPin != v.CAPin {
		return refuse(invitationRefused)
	}
	_, err := s.requireUpstreams(ctx, v.Scope)
	return err
}

func invitationTarget(q apicontract.UplinkInviteArgs, publicURL string) error {
	endpoint, e := apicontract.ParseServerAddress(q.ServerURL)
	if e != nil {
		return bad("server_url")
	}
	if endpoint.Scheme != "https" {
		return bad("server_url")
	}
	if e = invitationAddress(endpoint, publicURL); e != nil {
		return e
	}
	digest, e := hex.DecodeString(q.SecretDigest)
	if e != nil || len(digest) != 32 || strings.ToLower(q.SecretDigest) != q.SecretDigest {
		return bad("secret_digest")
	}
	return nil
}

func invitationAddress(endpoint *url.URL, publicURL string) error {
	public, err := url.Parse(publicURL)
	if err != nil || public.Scheme != "https" || public.Hostname() == "" {
		return refuse(unavailable)
	}
	if apicontract.SameServerAddress(endpoint.String(), publicURL) {
		return nil
	}
	problem := refuse(invitationAddressMismatch)
	problem.Detail = "Public address: " + publicURL
	return problem
}
