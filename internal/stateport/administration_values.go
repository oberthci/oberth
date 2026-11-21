package stateport

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
)

// AdministrativeFilter selects an exclusive descending page after scope filtering.
type AdministrativeFilter struct {
	Action, Actor, Since, Until string
	Before                      int64
	Limit                       int
}

// AuditRecord carries the immutable action and its stored chain links.
type AuditRecord struct {
	model.AuditAction
	SHA256, PreviousSHA256 string
}

func (f AdministrativeFilter) Bounds() (time.Time, time.Time, error) {
	var since, until time.Time
	var err error
	if f.Since != "" {
		since, err = time.Parse(time.RFC3339Nano, f.Since)
	}
	if err == nil && f.Until != "" {
		until, err = time.Parse(time.RFC3339Nano, f.Until)
	}
	if err != nil || f.Before < 0 || f.Limit < 1 || f.Limit > 200 || !since.IsZero() && !until.IsZero() && since.After(until) {
		return since, until, publicerr.New(publicerr.InvalidInput, "The audit filter is invalid.", "Use valid bounds and a page of at most 200 records.", err)
	}
	return since, until, nil
}

// AuditDigest is shared by durable and in-memory audit chain implementations.
func AuditDigest(domain, previous string, action model.AuditAction) string {
	body, _ := json.Marshal(action)
	return fmt.Sprintf("%x", sha256.Sum256(append([]byte(domain+"\x00"+previous+"\x00"), body...)))
}

func AuditGenesis() string { return fmt.Sprintf("%x", sha256.Sum256(nil)) }
