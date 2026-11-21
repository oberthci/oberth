package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"github.com/oberthci/oberth/internal/apicontract"
	"golang.org/x/crypto/ssh"
	"io"
	"net/http/httptrace"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"time"

	"github.com/oberthci/oberth/internal/clientcert"
	"github.com/oberthci/oberth/internal/model"
)

// UplinkPushKeyFile names the Git private key written into a joined profile.
const UplinkPushKeyFile = "push.key"

// HostKeysFile names the installation's SSH host keys in a joined profile.
const HostKeysFile = "known_hosts"

// InvitationFileBytes bounds a whole invitation file; its host-key record may
// use half of it, so every file an issuer writes is one join reads.
const InvitationFileBytes = 16 << 10

// Join publishes a complete profile only after verifying one redemption.
func Join(ctx context.Context, cfg Config, invitation model.InvitationFile, dir string) (model.InviteCertificate, error) {
	var result model.InviteCertificate
	if !cfg.Time().Before(invitation.ExpiresAt) || invitation.Secret == "" || validateAddress(invitation.ServerURL) != nil {
		return result, failure("invitation", nil)
	}
	if !ValidHostKeys(invitation.KnownHosts) {
		return result, failure("invitation_hosts", nil)
	}
	stage, err := stageJoin(dir)
	if err != nil {
		return result, err
	}
	defer cleanupJoin(stage)
	return joinPrepared(ctx, cfg, invitation, joinPaths{stage, dir})
}

type joinPaths struct{ stage, dir string }

func joinPrepared(ctx context.Context, cfg Config, invitation model.InvitationFile, paths joinPaths) (model.InviteCertificate, error) {
	var result model.InviteCertificate
	stage, dir := paths.stage, paths.dir
	keys, err := prepareJoin(stage, invitation)
	if err != nil {
		return result, err
	}

	profile, keep, err := joinSelection(filepath.Dir(dir), invitation)
	if err != nil {
		return result, err
	}
	profile.KnownHosts = filepath.Join(dir, HostKeysFile)
	selection, temp, err := reserveSelection(filepath.Dir(dir), filepath.Base(dir))
	if err != nil {
		return result, err
	}
	defer selection.Close()
	defer func() { _ = selection.Remove(temp) }()
	result, ca, err := redeem(ctx, cfg, invitation, keys)
	if err != nil {
		return result, err
	}
	if err = verifyEnrolled(result, invitation, enrolledMaterial{keys, ca, cfg.Time()}); err != nil {
		return result, failure("join_unknown", err)
	}
	if err = saveJoinFiles(stage, result, ca, profile); err != nil {
		return result, failure("join_unknown", err)
	}
	if err = publishJoin(stage, dir); err != nil {
		return result, failure("join_unknown", err)
	}
	if err = commitJoinSelection(selection, temp, keep); err != nil {
		cleanupJoin(dir)
		return result, failure("join_unknown", err)
	}
	return result, nil
}
func prepareJoinFiles(dir string, values map[string][]byte) error {
	for name := range values {
		if err := ReservePrivate(filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	for name, data := range values {
		if len(data) > 0 {
			if err := WritePrivate(filepath.Join(dir, name), data); err != nil {
				return err
			}
		}
	}
	return nil
}

func redeem(ctx context.Context, cfg Config, invitation model.InvitationFile, keys localKeys) (model.InviteCertificate, []byte, error) {
	var result model.InviteCertificate
	cfg.Options = Options{URL: invitation.ServerURL}
	if cfg.Transport == nil {
		rt, err := transport(cfg.Options)
		if err != nil {
			return result, nil, err
		}
		rt.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true, VerifyConnection: func(state tls.ConnectionState) error { return verifyInvitationPeer(state, invitation, cfg.Time()) }}
		cfg.Transport = rt
	}
	c, err := New(cfg)
	if err != nil {
		return result, nil, err
	}
	defer c.Close()
	c.endpoint += "/enroll"
	args := model.InviteRedemption{Secret: invitation.Secret, ServerURL: invitation.ServerURL, CAPin: invitation.CAPin, CSR: string(keys.csr), SSHPublicKey: string(keys.pushPublic)}
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "uplink_redeem", "arguments": args}})
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var wrote atomic.Bool
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{WroteRequest: func(httptrace.WroteRequestInfo) { wrote.Store(true) }})
	response, err := c.request(ctx, body)
	if err != nil {
		return result, nil, joinRequestFailure(err, wrote.Load(), invitation.ServerURL)
	}
	defer response.Body.Close()
	data, err := apicontract.ReadReply(response, apicontract.RPCReplyBytes(apicontract.ResponseBytes))
	if err != nil {
		return result, nil, failure("join_unknown", err)
	}
	payload, refused, _, err := apicontract.ToolPayload(data)
	if err != nil {
		return result, nil, failure("join_unknown", err)
	}
	if refused {
		return result, nil, resultError(payload)
	}
	if json.Unmarshal(payload, &result) != nil {
		return result, nil, failure("join_unknown", nil)
	}
	if response.TLS == nil || len(response.TLS.PeerCertificates) == 0 {
		return result, nil, failure("join_peer", nil)
	}
	root := response.TLS.PeerCertificates[len(response.TLS.PeerCertificates)-1]
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root.Raw})
	return result, ca, nil
}

func verifyInvitationPeer(state tls.ConnectionState, invitation model.InvitationFile, now time.Time) error {
	if len(state.PeerCertificates) < 2 {
		return failure("join_peer", nil)
	}
	root := state.PeerCertificates[len(state.PeerCertificates)-1]
	if clientcert.Pin(root) != invitation.CAPin {
		return failure("join_peer", nil)
	}
	roots := x509.NewCertPool()
	roots.AddCert(root)
	intermediates := x509.NewCertPool()
	for _, cert := range state.PeerCertificates[1 : len(state.PeerCertificates)-1] {
		intermediates.AddCert(cert)
	}
	u, _ := url.Parse(invitation.ServerURL)
	_, err := state.PeerCertificates[0].Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates, DNSName: u.Hostname(), CurrentTime: now})
	if err != nil {
		return failure("join_peer", err)
	}
	return nil
}

type enrolledMaterial struct {
	keys localKeys
	ca   []byte
	now  time.Time
}

func verifyEnrolled(result model.InviteCertificate, invitation model.InvitationFile, material enrolledMaterial) error {
	if _, err := ValidateSSHEndpoint(result.SSHURL, invitation.ServerURL); err != nil {
		return err
	}
	keys, ca, now := material.keys, material.ca, material.now
	cert, err := clientcert.ParseCertificate([]byte(result.Certificate))
	if err != nil {
		return err
	}
	expected, _ := x509.MarshalPKIXPublicKey(keys.public)
	if !bytes.Equal(cert.RawSubjectPublicKeyInfo, expected) || cert.Subject.CommonName != invitation.Identity || result.Identity != invitation.Identity || !slices.Equal(result.Scope, invitation.Scope) || !clientUsage(cert, now) {
		return failure("invitation", nil)
	}
	root, err := clientcert.ParseCertificate(ca)
	if err != nil || clientcert.Pin(root) != invitation.CAPin {
		return failure("invitation", err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(root)
	_, err = cert.Verify(x509.VerifyOptions{Roots: roots, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	return err
}

func prepareJoin(dir string, invitation model.InvitationFile) (localKeys, error) {
	if err := os.MkdirAll(filepath.Dir(dir), 0700); err != nil {
		return localKeys{}, fileFailure(filepath.Dir(dir), "write", err)
	}
	parent, err := directory(filepath.Dir(dir), true)
	if err != nil {
		return localKeys{}, err
	}
	_ = parent.Close()
	keys, err := generateKeys(invitation.Identity)
	if err != nil {
		return localKeys{}, err
	}
	values := map[string][]byte{"admin.key": keys.key, "admin.csr": keys.csr, UplinkPushKeyFile: keys.pushKey, "push.pub": keys.pushPublic, HostKeysFile: []byte(invitation.KnownHosts), "admin.crt": nil, "ca.crt": nil, "installation.json": nil}
	if err = prepareJoinFiles(dir, values); err != nil {
		return localKeys{}, err
	}
	return keys, nil
}

func saveJoinFiles(dir string, result model.InviteCertificate, ca []byte, metadata Profile) error {
	metadata.SSHURL = result.SSHURL
	profile, _ := json.Marshal(metadata)
	for name, data := range map[string][]byte{"admin.crt": []byte(result.Certificate), "ca.crt": ca, "installation.json": profile} {
		if err := WritePrivate(filepath.Join(dir, name), data); err != nil {
			return err
		}
	}
	return nil
}

// ValidHostKeys reports whether text holds at least one known_hosts entry,
// nothing that is not one, and fits its share of the invitation file.
func ValidHostKeys(text string) bool {
	if len(text) > InvitationFileBytes/2 {
		return false
	}
	rest, found := []byte(text), false
	for {
		_, _, _, _, next, err := ssh.ParseKnownHosts(rest)
		if errors.Is(err, io.EOF) {
			return found
		}
		if err != nil {
			return false
		}
		rest, found = next, true
	}
}

func clientUsage(cert *x509.Certificate, now time.Time) bool {
	return !cert.IsCA && len(cert.ExtKeyUsage) == 1 && cert.ExtKeyUsage[0] == x509.ExtKeyUsageClientAuth && len(cert.UnknownExtKeyUsage) == 0 && !now.Before(cert.NotBefore) && now.Before(cert.NotAfter)
}
