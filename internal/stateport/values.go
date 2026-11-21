package stateport

import (
	"encoding/hex"
	"strings"

	"github.com/oberthci/oberth/internal/publicerr"
)

const (
	DefaultMaxBytes int64 = 1 << 30
	HardMaxBytes    int64 = 4 << 30
)

// Protocol is configured explicitly, never inferred from archived rows.
type Protocol struct {
	SchemaIdentity string `json:"schema_identity"`
	AuditDomain    string `json:"audit_domain"`
}

// DefaultProtocol supplies the explicit identities for a fresh installation.
func DefaultProtocol() Protocol {
	return Protocol{SchemaIdentity: "oberth-schema-v1", AuditDomain: "oberth-audit-v2"}
}

// Validate checks bounded explicit identities without reading database rows.
func (p Protocol) Validate() error {
	if !protocolToken(p.SchemaIdentity) || !protocolToken(p.AuditDomain) {
		return publicerr.New(publicerr.InvalidInput, "protocol identity is missing or invalid", "Supply schema identity and audit domain tokens of at most 128 bytes.", nil)
	}
	return nil
}

func protocolToken(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for index, char := range value {
		alnum := char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9'
		if !alnum && (index == 0 || !strings.ContainsRune("._:-", char)) {
			return false
		}
	}
	return true
}

// AuditHead identifies the last verified action and its chain digest.
type AuditHead struct {
	ID     int64  `json:"id"`
	SHA256 string `json:"sha256"`
}

// BackupReport contains local verification facts, never database rows or
// rollback authorization; C9 binds the selected backup and configured continuity.
type BackupReport struct {
	Schema      int       `json:"schema"`
	AuditHead   AuditHead `json:"audit_head"`
	Integrity   bool      `json:"integrity"`
	ForeignKeys bool      `json:"foreign_keys"`
}

// Validate accepts only positive schema and complete successful verification.
func (r BackupReport) Validate() error {
	if r.Schema <= 0 || r.AuditHead.ID < 0 || !digest(r.AuditHead.SHA256) || !r.Integrity || !r.ForeignKeys {
		return publicerr.New(publicerr.SchemaRefused, "database verification is incomplete or failed", "Use a database with a valid schema, audit chain, integrity and foreign keys.", nil)
	}
	return nil
}

// Options binds an operation to the expected protocol, schema and byte limit.
type Options struct {
	Protocol Protocol `json:"protocol"`
	Schema   int      `json:"schema"`
	MaxBytes int64    `json:"max_bytes"`
}

// Validate refuses missing identity/schema and unsupported byte limits.
func (o Options) Validate() error {
	if err := o.Protocol.Validate(); err != nil {
		return err
	}
	if o.Schema <= 0 {
		return publicerr.New(publicerr.InvalidInput, "expected schema is missing", "Supply the schema supported by this executable.", nil)
	}
	_, err := o.Limit()
	return err
}

// Limit returns the logical database limit. Zero chooses 1 GiB; overrides must
// be positive and at most 4 GiB. Archive framing is bounded separately.
func (o Options) Limit() (int64, error) {
	if o.MaxBytes == 0 {
		return DefaultMaxBytes, nil
	}
	if o.MaxBytes < 0 || o.MaxBytes > HardMaxBytes {
		return 0, publicerr.New(publicerr.InvalidInput, "database byte limit is out of range", "Choose a positive limit at most 4 GiB, or zero for 1 GiB.", nil)
	}
	return o.MaxBytes, nil
}

func digest(value string) bool {
	body, err := hex.DecodeString(value)
	return err == nil && len(body) == 32 && strings.ToLower(value) == value
}
