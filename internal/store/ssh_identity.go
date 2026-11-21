package store

import (
	"context"
	"github.com/oberthci/oberth/internal/stateport"
)

// SSHKey reads the uplink lifetime independently of its enrolled credentials.
func (s *Store) SSHKey(ctx context.Context, fingerprint string) (stateport.Key, error) {
	k, err := s.Key(ctx, fingerprint)
	if err != nil || k.Revoked {
		return k, err
	}
	if !k.ExpiresAt.IsZero() && !s.options.Clock().Before(k.ExpiresAt) {
		return k, refuse(forbidden, nil)
	}
	return k, nil
}
