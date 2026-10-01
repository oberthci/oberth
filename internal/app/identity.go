package app

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

type UplinkCatalog interface {
	UplinkByFingerprint(context.Context, string) (model.Uplink, error)
}

// SSHIdentities adapts the durable uplink table to the SSH handshake. The SSH
// package intentionally exposes no database details and receives only the
// actor identity that must be attached to accepted Git events.
type SSHIdentities struct {
	Uplinks UplinkCatalog
	Timeout time.Duration
}

func (identities SSHIdentities) ResolveFingerprint(fingerprint string) (string, bool, error) {
	if identities.Uplinks == nil || strings.TrimSpace(fingerprint) == "" {
		return "", false, nil
	}
	timeout := identities.Timeout
	if timeout <= 0 {
		timeout = defaultCatalogTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	uplink, err := identities.Uplinks.UplinkByFingerprint(ctx, fingerprint)
	if errors.Is(err, store.ErrNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if strings.TrimSpace(uplink.Identity) == "" {
		return "", false, nil
	}
	return uplink.Identity, true, nil
}
