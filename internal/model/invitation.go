package model

import (
	"slices"
	"time"
)

// Invitation is a public, pinned enrollment binding. Possessing this value
// alone grants nothing; redemption also requires the private handoff secret.
type Invitation struct {
	ID        string    `json:"id"`
	Identity  string    `json:"identity"`
	ServerURL string    `json:"server_url"`
	CAPin     string    `json:"ca_pin"`
	Scope     []string  `json:"scope"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (v Invitation) Clone() Invitation { v.Scope = slices.Clone(v.Scope); return v }

// InvitationFile is the private enrollment handoff. Callers never put it in
// ordinary logs, audit details or public results. KnownHosts carries the
// installation's SSH host keys, copied by the issuer from its install record,
// so the joined profile trusts the Git endpoint as it trusts the CA pin.
type InvitationFile struct {
	Invitation
	KnownHosts string `json:"known_hosts"`
	Secret     string `json:"secret" sensitive:"true"`
}

func (v InvitationFile) Clone() InvitationFile { v.Invitation = v.Invitation.Clone(); return v }

type InviteRedemption struct {
	Secret       string `json:"secret" sensitive:"true"`
	ServerURL    string `json:"server_url"`
	CAPin        string `json:"ca_pin"`
	CSR          string `json:"csr"`
	SSHPublicKey string `json:"ssh_public_key"`
}

type InviteCertificate struct {
	Identity       string   `json:"identity"`
	Scope          []string `json:"scope"`
	SSHFingerprint string   `json:"ssh_fingerprint"`
	SSHURL         string   `json:"ssh_url"`
	Certificate    string   `json:"certificate"`
}

func (v InviteCertificate) Clone() InviteCertificate { v.Scope = slices.Clone(v.Scope); return v }
