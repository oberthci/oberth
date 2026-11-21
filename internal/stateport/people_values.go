package stateport

import (
	"github.com/oberthci/oberth/internal/model"
	"time"
)

type InvitationReservation struct {
	ID, Digest, Token string
	Until             time.Time
	Events            []EventWrite
}

type Invitation struct {
	Public model.Invitation
	Digest string
}

type Enrollment struct {
	Reservation                string
	Events                     []EventWrite
	InvitationID, Digest, SPKI string
	Key                        Key
	Credential                 Credential
}

type IssueLockRequest struct {
	ID    int64
	Owner string
}

type IssueClose struct {
	ID, Revision int64
	WorkID       string
}

type Key struct {
	Fingerprint, Identity, Kind string
	Admin, ParkOnPush, Revoked  bool
	Scope                       []string
	RevokedAt                   *time.Time
	ExpiresAt                   time.Time
}

type Credential struct {
	Digest, Fingerprint string
	ExpiresAt           time.Time
}

// IssueScope authorizes upstream task lists independently of registered repositories.
type IssueScope struct {
	Upstreams []string
	Global    bool
}

// CertificateBinding renews the uplink lifetime with its public certificate pin.
type CertificateBinding struct {
	Fingerprint, SPKI string
	ExpiresAt         time.Time
}
