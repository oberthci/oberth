package store

import (
	"context"
	"encoding/json"
)

func (s *Store) ReadCapability(ctx context.Context, repository string) (json.RawMessage, error) {
	var body string
	err := s.readers.QueryRowContext(ctx, `SELECT c.value FROM capabilities c
  JOIN repositories r ON r.id=c.repo_id WHERE r.canonical=? AND c.revoked=0`, repository).Scan(&body)
	return json.RawMessage(body), databaseError(err)
}

func (s *Store) SchedulePaused(ctx context.Context, repository, entry string) (bool, error) {
	var paused bool
	err := s.readers.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM schedule_pauses p
  JOIN repositories r ON r.id=p.repo_id WHERE r.canonical=? AND p.entry=? AND p.paused=1)`, repository, entry).Scan(&paused)
	return paused, databaseError(err)
}

func (s *Store) ClaimOwner(ctx context.Context, claim string) (ClaimOwner, error) {
	var owner ClaimOwner
	err := s.readers.QueryRowContext(ctx, `SELECT p.claim,r.canonical,p.tier FROM pvc_owners p
  JOIN repositories r ON r.id=p.repo_id WHERE p.claim=?`, claim).Scan(&owner.Claim, &owner.Repository, &owner.Tier)
	return owner, databaseError(err)
}
