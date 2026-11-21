package service

import (
	"context"
	"crypto/x509"
	"encoding/base64"

	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/stateport"
	"golang.org/x/crypto/ssh"
)

// InstallAdmin is a trusted startup operation, outside the public tool table.
// The server supplies the certificate and push public keys read from its Secret.
func (s *Service) InstallAdmin(ctx context.Context, certificateKey, pushKey []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.ctx.Err() != nil || s.c.People.Enrollment == nil {
		return refuse(unavailable)
	}
	key, err := publicSSHKey(certificateKey)
	if err != nil {
		return err
	}
	cryptoKey, ok := key.(ssh.CryptoPublicKey)
	if !ok {
		return bad("admin_public_key")
	}
	der, err := x509.MarshalPKIXPublicKey(cryptoKey.CryptoPublicKey())
	if err != nil {
		return bad("admin_public_key")
	}
	fingerprint, err := sshFingerprint(string(pushKey))
	if err != nil {
		return err
	}
	v := stateport.IdentityRegistration{Key: stateport.Key{Fingerprint: fingerprint, Identity: "install-admin", Kind: "installer", Admin: true}, SPKI: base64.StdEncoding.EncodeToString(der)}
	a := s.peopleAction(actor.Actor{Identity: "installation"}, "identity.changed", "install administrator", "")
	return s.c.People.Enrollment.InstallAdmin(ctx, a, v)
}
